package prometheus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ghodss/yaml"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	v1 "k8s.io/api/core/v1"
	kapierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	kapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/client/conditions"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	exutil "github.com/openshift/origin/test/extended/util"
)

const waitForPrometheusStartSeconds = 240

var _ = g.Describe("[Feature:Prometheus][Conformance] Prometheus", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("prometheus")

		url, bearerToken string
	)

	g.BeforeEach(func() {
		var ok bool
		url, bearerToken, ok = locatePrometheus(oc)
		if !ok {
			e2e.Skipf("Prometheus could not be located on this cluster, skipping prometheus test")
		}
	})

	g.Describe("when installed on the cluster", func() {
		g.It("should report telemetry if a cloud.openshift.com token is present", func() {
			if !hasPullSecret(oc.AdminKubeClient(), "cloud.openshift.com") {
				e2e.Skipf("Telemetry is disabled")
			}
			oc.SetupProject()
			ns := oc.Namespace()

			execPodName := e2e.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod", func(pod *v1.Pod) { pod.Spec.Containers[0].Image = "centos:7" })
			defer func() { oc.AdminKubeClient().Core().Pods(ns).Delete(execPodName, metav1.NewDeleteOptions(1)) }()

			tests := map[string][]metricTest{
				// should have successfully sent at least once to remote
				`metricsclient_request_send{client="federate_to",job="telemeter-client",status_code="200"}`: {metricTest{greaterThanEqual: true, value: 1}},
				// should have scraped some metrics from prometheus
				`federate_samples{job="telemeter-client"}`: {metricTest{greaterThanEqual: true, value: 10}},
			}
			runQueries(tests, oc, ns, execPodName, url, bearerToken)

			e2e.Logf("Telemetry is enabled: %s", bearerToken)
		})

		g.It("should start and expose a secured proxy and unsecured metrics", func() {
			oc.SetupProject()
			ns := oc.Namespace()
			execPodName := e2e.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod", func(pod *v1.Pod) { pod.Spec.Containers[0].Image = "centos:7" })
			defer func() { oc.AdminKubeClient().Core().Pods(ns).Delete(execPodName, metav1.NewDeleteOptions(1)) }()

			g.By("checking the unsecured metrics path")
			var metrics map[string]*dto.MetricFamily
			o.Expect(wait.PollImmediate(10*time.Second, waitForPrometheusStartSeconds*time.Second, func() (bool, error) {
				results, err := getInsecureURLViaPod(ns, execPodName, fmt.Sprintf("%s/metrics", url))
				if err != nil {
					e2e.Logf("unable to get unsecured metrics: %v", err)
					return false, nil
				}

				p := expfmt.TextParser{}
				metrics, err = p.TextToMetricFamilies(bytes.NewBufferString(results))
				o.Expect(err).NotTo(o.HaveOccurred())
				// original field in 2.0.0-beta
				counts := findCountersWithLabels(metrics["tsdb_samples_appended_total"], labels{})
				if len(counts) != 0 && counts[0] > 0 {
					return true, nil
				}
				// 2.0.0-rc.0
				counts = findCountersWithLabels(metrics["tsdb_head_samples_appended_total"], labels{})
				if len(counts) != 0 && counts[0] > 0 {
					return true, nil
				}
				// 2.0.0-rc.2
				counts = findCountersWithLabels(metrics["prometheus_tsdb_head_samples_appended_total"], labels{})
				if len(counts) != 0 && counts[0] > 0 {
					return true, nil
				}
				return false, nil
			})).NotTo(o.HaveOccurred(), fmt.Sprintf("Did not find tsdb_samples_appended_total, tsdb_head_samples_appended_total, or prometheus_tsdb_head_samples_appended_total"))

			g.By("verifying the oauth-proxy reports a 403 on the root URL")
			err := expectURLStatusCodeExec(ns, execPodName, url, 403)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("verifying a service account token is able to authenticate")
			err = expectBearerTokenURLStatusCodeExec(ns, execPodName, fmt.Sprintf("%s/graph", url), bearerToken, 200)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("verifying a service account token is able to access the Prometheus API")
			// expect all endpoints within 60 seconds
			var lastErrs []error
			o.Expect(wait.PollImmediate(10*time.Second, 2*time.Minute, func() (bool, error) {
				contents, err := getBearerTokenURLViaPod(ns, execPodName, fmt.Sprintf("%s/api/v1/targets", url), bearerToken)
				o.Expect(err).NotTo(o.HaveOccurred())

				targets := &prometheusTargets{}
				err = json.Unmarshal([]byte(contents), targets)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("verifying all expected jobs have a working target")
				lastErrs = all(
					targets.Expect(labels{"job": "apiserver"}, "up", "^https://.*/metrics$"),
					// TODO: add openshift api
					// TODO: this should be https
					targets.Expect(labels{"job": "scheduler"}, "up", "^http://.*/metrics$"),
					// TODO: this should be https
					targets.Expect(labels{"job": "kube-controller-manager"}, "up", "^http://.*/metrics$"),
					targets.Expect(labels{"job": "kube-state-metrics"}, "up", "^https://.*/metrics$"),
					// TODO: should probably be https
					targets.Expect(labels{"job": "cluster-version-operator"}, "up", "^http://.*/metrics$"),
					targets.Expect(labels{"job": "prometheus-k8s", "namespace": "openshift-monitoring", "pod": "prometheus-k8s-0"}, "up", "^https://.*/metrics$"),
					targets.Expect(labels{"job": "kubelet"}, "up", "^https://.*/metrics$"),
					targets.Expect(labels{"job": "kubelet"}, "up", "^https://.*/metrics/cadvisor$"),
					targets.Expect(labels{"job": "node-exporter"}, "up", "^https://.*/metrics$"),
				)
				if len(lastErrs) > 0 {
					e2e.Logf("missing some targets: %v", lastErrs)
					return false, nil
				}
				return true, nil
			})).NotTo(o.HaveOccurred())
		})
	})
})

func all(errs ...error) []error {
	var result []error
	for _, err := range errs {
		if err != nil {
			result = append(result, err)
		}
	}
	return result
}

type prometheusTargets struct {
	Data struct {
		ActiveTargets []struct {
			Labels    map[string]string
			Health    string
			ScrapeUrl string
		}
	}
	Status string
}

func (t *prometheusTargets) Expect(l labels, health, scrapeURLPattern string) error {
	for _, target := range t.Data.ActiveTargets {
		match := true
		for k, v := range l {
			if target.Labels[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if health != target.Health {
			continue
		}
		if !regexp.MustCompile(scrapeURLPattern).MatchString(target.ScrapeUrl) {
			continue
		}
		return nil
	}
	return fmt.Errorf("no match for %v with health %s and scrape URL %s", l, health, scrapeURLPattern)
}

type labels map[string]string

func (l labels) With(name, value string) labels {
	n := make(labels)
	for k, v := range l {
		n[k] = v
	}
	n[name] = value
	return n
}

func findEnvVar(vars []kapi.EnvVar, key string) string {
	for _, v := range vars {
		if v.Name == key {
			return v.Value
		}
	}
	return ""
}

func findMetricsWithLabels(f *dto.MetricFamily, labels map[string]string) []*dto.Metric {
	var result []*dto.Metric
	if f == nil {
		return result
	}
	for _, m := range f.Metric {
		matched := map[string]struct{}{}
		for _, l := range m.Label {
			if expect, ok := labels[l.GetName()]; ok {
				if expect != l.GetValue() {
					break
				}
				matched[l.GetName()] = struct{}{}
			}
		}
		if len(matched) != len(labels) {
			continue
		}
		result = append(result, m)
	}
	return result
}

func findCountersWithLabels(f *dto.MetricFamily, labels map[string]string) []float64 {
	var result []float64
	for _, m := range findMetricsWithLabels(f, labels) {
		result = append(result, m.Counter.GetValue())
	}
	return result
}

func findGaugesWithLabels(f *dto.MetricFamily, labels map[string]string) []float64 {
	var result []float64
	for _, m := range findMetricsWithLabels(f, labels) {
		result = append(result, m.Gauge.GetValue())
	}
	return result
}

func findMetricLabels(f *dto.MetricFamily, labels map[string]string, match string) []string {
	var result []string
	for _, m := range findMetricsWithLabels(f, labels) {
		for _, l := range m.Label {
			if l.GetName() == match {
				result = append(result, l.GetValue())
				break
			}
		}
	}
	return result
}

func expectURLStatusCodeExec(ns, execPodName, url string, statusCode int) error {
	cmd := fmt.Sprintf("curl -k -s -o /dev/null -w '%%{http_code}' %q", url)
	output, err := e2e.RunHostCmd(ns, execPodName, cmd)
	if err != nil {
		return fmt.Errorf("host command failed: %v\n%s", err, output)
	}
	if output != strconv.Itoa(statusCode) {
		return fmt.Errorf("last response from server was not %d: %s", statusCode, output)
	}
	return nil
}

func expectBearerTokenURLStatusCodeExec(ns, execPodName, url, bearer string, statusCode int) error {
	cmd := fmt.Sprintf("curl -k -s -H 'Authorization: Bearer %s' -o /dev/null -w '%%{http_code}' %q", bearer, url)
	output, err := e2e.RunHostCmd(ns, execPodName, cmd)
	if err != nil {
		return fmt.Errorf("host command failed: %v\n%s", err, output)
	}
	if output != strconv.Itoa(statusCode) {
		return fmt.Errorf("last response from server was not %d: %s", statusCode, output)
	}
	return nil
}

func getBearerTokenURLViaPod(ns, execPodName, url, bearer string) (string, error) {
	cmd := fmt.Sprintf("curl -s -k -H 'Authorization: Bearer %s' %q", bearer, url)
	output, err := e2e.RunHostCmd(ns, execPodName, cmd)
	if err != nil {
		return "", fmt.Errorf("host command failed: %v\n%s", err, output)
	}
	return output, nil
}

func getAuthenticatedURLViaPod(ns, execPodName, url, user, pass string) (string, error) {
	cmd := fmt.Sprintf("curl -s -u %s:%s %q", user, pass, url)
	output, err := e2e.RunHostCmd(ns, execPodName, cmd)
	if err != nil {
		return "", fmt.Errorf("host command failed: %v\n%s", err, output)
	}
	return output, nil
}

func getInsecureURLViaPod(ns, execPodName, url string) (string, error) {
	cmd := fmt.Sprintf("curl -s -k %q", url)
	output, err := e2e.RunHostCmd(ns, execPodName, cmd)
	if err != nil {
		return "", fmt.Errorf("host command failed: %v\n%s", err, output)
	}
	return output, nil
}

func waitForServiceAccountInNamespace(c clientset.Interface, ns, serviceAccountName string, timeout time.Duration) error {
	w, err := c.Core().ServiceAccounts(ns).Watch(metav1.SingleObject(metav1.ObjectMeta{Name: serviceAccountName}))
	if err != nil {
		return err
	}
	_, err = watch.Until(timeout, w, conditions.ServiceAccountHasSecrets)
	return err
}

func locatePrometheus(oc *exutil.CLI) (url, bearerToken string, ok bool) {
	_, err := oc.AdminKubeClient().Core().Services("openshift-monitoring").Get("prometheus-k8s", metav1.GetOptions{})
	if kapierrs.IsNotFound(err) {
		return "", "", false
	}

	waitForServiceAccountInNamespace(oc.AdminKubeClient(), "openshift-monitoring", "prometheus-k8s", 2*time.Minute)
	for i := 0; i < 30; i++ {
		secrets, err := oc.AdminKubeClient().Core().Secrets("openshift-monitoring").List(metav1.ListOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, secret := range secrets.Items {
			if secret.Type != v1.SecretTypeServiceAccountToken {
				continue
			}
			if !strings.HasPrefix(secret.Name, "prometheus-") {
				continue
			}
			bearerToken = string(secret.Data[v1.ServiceAccountTokenKey])
			break
		}
		if len(bearerToken) == 0 {
			e2e.Logf("Waiting for prometheus service account secret to show up")
			time.Sleep(time.Second)
			continue
		}
	}
	o.Expect(bearerToken).ToNot(o.BeEmpty())

	return "https://prometheus-k8s.openshift-monitoring.svc:9091", bearerToken, true
}

func hasPullSecret(client clientset.Interface, name string) bool {
	cm, err := client.CoreV1().ConfigMaps("kube-system").Get("cluster-config-v1", metav1.GetOptions{})
	if err != nil {
		if kapierrs.IsNotFound(err) {
			return false
		}
		e2e.Failf("could not retrieve cluster-config-v1: %v", err)
	}
	ic := make(map[string]interface{})
	if err := yaml.Unmarshal([]byte(cm.Data["install-config"]), &ic); err != nil {
		e2e.Failf("could not parse cluster-config-v1 install-config: %v", err)
	}

	ps := struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}{}
	if _, ok := ic["pullSecret"].(string); !ok {
		e2e.Failf("could not get pullSecret from cluster-config-v1: %v", err)
	}
	if err := json.Unmarshal([]byte(ic["pullSecret"].(string)), &ps); err != nil {
		e2e.Failf("could not unmashal pullSecret from cluster-config-v1: %v", err)
	}
	return len(ps.Auths[name].Auth) > 0
}
