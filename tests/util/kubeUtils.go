// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/golang/sync/errgroup"
	multierror "github.com/hashicorp/go-multierror"
	"golang.org/x/net/context/ctxhttp"

	"istio.io/istio/pkg/log"
)

const (
	podFailedGet = "Failed_Get"
	// The index of STATUS field in kubectl CLI output.
	statusField = 2
)

var (
	logDumpResources = []string{
		"pod",
		"service",
		"ingress",
	}
)

// Fill complete a template with given values and generate a new output file
func Fill(outFile, inFile string, values interface{}) error {
	var bytes bytes.Buffer
	w := bufio.NewWriter(&bytes)
	tmpl, err := template.ParseFiles(inFile)
	if err != nil {
		return err
	}

	if err := tmpl.Execute(w, values); err != nil {
		return err
	}

	if err := w.Flush(); err != nil {
		return err
	}

	if err := ioutil.WriteFile(outFile, bytes.Bytes(), 0644); err != nil {
		return err
	}
	log.Infof("Created %s from template %s", outFile, inFile)
	return nil
}

// CreateNamespace create a kubernetes namespace
func CreateNamespace(n string) error {
	if _, err := Shell("kubectl create namespace %s", n); err != nil {
		return err
	}
	log.Infof("namespace %s created\n", n)
	return nil
}

// DeleteNamespace delete a kubernetes namespace
func DeleteNamespace(n string) error {
	_, err := Shell("kubectl delete namespace %s", n)
	return err
}

// NamespaceDeleted check if a kubernete namespace is deleted
func NamespaceDeleted(n string) (bool, error) {
	output, err := ShellSilent("kubectl get namespace %s -o name", n)
	if strings.Contains(output, "NotFound") {
		return true, nil
	}
	return false, err
}

// KubeApplyContents kubectl apply from contents
func KubeApplyContents(namespace, yamlContents string) error {
	tmpfile, err := WriteTempfile(os.TempDir(), "kubeapply", ".yaml", yamlContents)
	if err != nil {
		return err
	}
	defer removeFile(tmpfile)
	return KubeApply(namespace, tmpfile)
}

// KubeApply kubectl apply from file
func KubeApply(namespace, yamlFileName string) error {
	_, err := Shell("kubectl apply -n %s -f %s", namespace, yamlFileName)
	return err
}

// KubeDeleteContents kubectl apply from contents
func KubeDeleteContents(namespace, yamlContents string) error {
	tmpfile, err := WriteTempfile(os.TempDir(), "kubedelete", ".yaml", yamlContents)
	if err != nil {
		return err
	}
	defer removeFile(tmpfile)
	return KubeDelete(namespace, tmpfile)
}

func removeFile(path string) {
	err := os.Remove(path)
	if err != nil {
		log.Errorf("Unable to remove %s: %v", path, err)
	}
}

// KubeDelete kubectl delete from file
func KubeDelete(namespace, yamlFileName string) error {
	_, err := Shell("kubectl delete -n %s -f %s", namespace, yamlFileName)
	return err
}

// GetIngress get istio ingress ip
func GetIngress(n string) (string, error) {
	retry := Retrier{
		BaseDelay: 1 * time.Second,
		MaxDelay:  1 * time.Second,
		Retries:   300, // ~5 minutes
	}
	ri := regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$`)
	//rp := regexp.MustCompile(`^[0-9]{1,5}$`) # Uncomment for minikube
	var ingress string
	retryFn := func(_ context.Context, i int) error {
		ip, err := ShellSilent("kubectl get svc istio-ingress -n %s -o jsonpath='{.status.loadBalancer.ingress[*].ip}'", n)
		// For minikube, comment out the previous line and uncomment the following line
		//ip, err := Shell("kubectl get po -l istio=ingress -n %s -o jsonpath='{.items[0].status.hostIP}'", n)
		if err != nil {
			return err
		}
		ip = strings.Trim(ip, "'")
		if ri.FindString(ip) == "" {
			return errors.New("ingress ip not available yet")
		}
		ingress = ip
		// For minikube, comment out the previous line and uncomment the following lines
		//port, e := Shell("kubectl get svc istio-ingress -n %s -o jsonpath='{.spec.ports[0].nodePort}'", n)
		//if e != nil {
		//	return e
		//}
		//port = strings.Trim(port, "'")
		//if rp.FindString(port) == "" {
		//	err = fmt.Errorf("unable to find ingress port")
		//	log.Warn(err)
		//	return err
		//}
		//ingress = ip + ":" + port
		log.Infof("Istio ingress: %s", ingress)

		return nil
	}

	ctx := context.Background()

	log.Info("Waiting for istio-ingress to get external IP")
	if _, err := retry.Retry(ctx, retryFn); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 5 * time.Second}
	ingressURL := fmt.Sprintf("http://%s", ingress)
	log.Infof("Sanity checking %v", ingressURL)
	for {
		select {
		case <-ctx.Done():
			return "", errors.New("istio-ingress readiness check timed out")
		default:
			response, err := ctxhttp.Get(ctx, client, ingressURL)
			if err == nil {
				log.Infof("Response %v %q received from %v", response.StatusCode, response.Status, ingressURL)
				return ingress, nil
			}
		}
	}
}

// GetIngressPod get istio ingress ip
func GetIngressPod(n string) (string, error) {
	retry := Retrier{
		BaseDelay: 5 * time.Second,
		MaxDelay:  5 * time.Minute,
		Retries:   20,
	}
	ipRegex := regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$`)
	portRegex := regexp.MustCompile(`^[0-9]+$`)
	var ingress string
	retryFn := func(_ context.Context, i int) error {
		podIP, err := Shell("kubectl get pod -l istio=ingress "+
			"-n %s -o jsonpath='{.items[0].status.hostIP}'", n)
		if err != nil {
			return err
		}
		podPort, err := Shell("kubectl get svc istio-ingress "+
			"-n %s -o jsonpath='{.spec.ports[0].nodePort}'", n)
		if err != nil {
			return err
		}
		podIP = strings.Trim(podIP, "'")
		podPort = strings.Trim(podPort, "'")
		if ipRegex.FindString(podIP) == "" {
			err = errors.New("unable to find ingress pod ip")
			log.Warna(err)
			return err
		}
		if portRegex.FindString(podPort) == "" {
			err = errors.New("unable to find ingress pod port")
			log.Warna(err)
			return err
		}
		ingress = fmt.Sprintf("%s:%s", podIP, podPort)
		log.Infof("Istio ingress: %s\n", ingress)
		return nil
	}
	_, err := retry.Retry(context.Background(), retryFn)
	return ingress, err
}

// GetPodsName gets names of all pods in specific namespace and return in a slice
func GetPodsName(n string) (pods []string) {
	res, err := Shell("kubectl -n %s get pods -o jsonpath='{.items[*].metadata.name}'", n)
	if err != nil {
		log.Infof("Failed to get pods name in namespace %s: %s", n, err)
		return
	}
	res = strings.Trim(res, "'")
	pods = strings.Split(res, " ")
	log.Infof("Existing pods: %v", pods)
	return
}

// GetPodStatus gets status of a pod from a namespace
// Note: It is not enough to check pod phase, which only implies there is at
// least one container running. Use kubectl CLI to get status so that we can
// ensure that all containers are running.
func GetPodStatus(n, pod string) string {
	status, err := Shell("kubectl -n %s get pods %s --no-headers", n, pod)
	if err != nil {
		log.Infof("Failed to get status of pod %s in namespace %s: %s", pod, n, err)
		status = podFailedGet
	}
	f := strings.Fields(status)
	if len(f) > statusField {
		return f[statusField]
	}
	return ""
}

// CheckPodsRunningWithMaxDuration returns if all pods in a namespace are in "Running" status
// Also check container status to be running.
func CheckPodsRunningWithMaxDuration(n string, maxDuration time.Duration) (ready bool) {
	if err := WaitForDeploymentsReady(n, maxDuration); err != nil {
		log.Errorf("CheckPodsRunning: %v", err.Error())
		return false
	}

	return true
}

// CheckPodsRunning returns readiness of all pods within a namespace. It will wait for upto 2 mins.
// use WithMaxDuration to specify a duration.
func CheckPodsRunning(n string) (ready bool) {
	return CheckPodsRunningWithMaxDuration(n, 2*time.Minute)
}

// CheckDeployment gets status of a deployment from a namespace
func CheckDeployment(ctx context.Context, namespace, deployment string) error {
	errc := make(chan error)
	go func() {
		if _, err := ShellMuteOutput("kubectl -n %s rollout status %s", namespace, deployment); err != nil {
			errc <- fmt.Errorf("%s in namespace %s failed", deployment, namespace)
		}
		errc <- nil
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CheckDeployments checks whether all deployment in a given namespace
func CheckDeployments(namespace string, timeout time.Duration) error {
	// wait for istio-system deployments to be fully rolled out before proceeding
	out, err := Shell("kubectl -n %s get deployment -o name", namespace)
	if err != nil {
		return fmt.Errorf("could not list deployments in namespace %q", namespace)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	g, ctx := errgroup.WithContext(ctx)
	deployments := strings.Fields(out)
	for i := range deployments {
		deployment := deployments[i]
		g.Go(func() error { return CheckDeployment(ctx, namespace, deployment) })
	}
	return g.Wait()
}

// FetchAndSaveClusterLogs will dump the logs for a cluster.
func FetchAndSaveClusterLogs(namespace string, tempDir string) error {
	var multiErr error
	fetchAndWrite := func(pod string) error {
		cmd := fmt.Sprintf(
			"kubectl get pods -n %s %s -o jsonpath={.spec.containers[*].name}", namespace, pod)
		containersString, err := Shell(cmd)
		if err != nil {
			return err
		}
		containers := strings.Split(containersString, " ")
		for _, container := range containers {
			filePath := filepath.Join(tempDir, fmt.Sprintf("%s_container:%s.log", pod, container))
			f, err := os.Create(filePath)
			if err != nil {
				return err
			}
			defer func() {
				if err = f.Close(); err != nil {
					log.Warnf("Error during closing file: %v\n", err)
				}
			}()
			dump, err := ShellMuteOutput(
				fmt.Sprintf("kubectl logs %s -n %s -c %s", pod, namespace, container))
			if err != nil {
				return err
			}
			if _, err = f.WriteString(fmt.Sprintf("%s\n", dump)); err != nil {
				return err
			}
		}
		return nil
	}

	_, err := Shell("kubectl get ingress --all-namespaces")
	if err != nil {
		return err
	}
	lines, err := Shell("kubectl get pods -n " + namespace)
	if err != nil {
		return err
	}
	pods := strings.Split(lines, "\n")
	if len(pods) > 1 {
		for _, line := range pods[1:] {
			if idxEndOfPodName := strings.Index(line, " "); idxEndOfPodName > 0 {
				pod := line[:idxEndOfPodName]
				log.Infof("Fetching logs on %s", pod)
				if err := fetchAndWrite(pod); err != nil {
					multiErr = multierror.Append(multiErr, err)
				}
			}
		}
	}

	for _, resrc := range logDumpResources {
		log.Info(fmt.Sprintf("Fetching deployment info on %s\n", resrc))
		filePath := filepath.Join(tempDir, fmt.Sprintf("%s.yaml", resrc))
		if yaml, err0 := ShellMuteOutput(
			fmt.Sprintf("kubectl get %s -n %s -o yaml", resrc, namespace)); err0 != nil {
			multiErr = multierror.Append(multiErr, err0)
		} else {
			if f, err1 := os.Create(filePath); err1 != nil {
				multiErr = multierror.Append(multiErr, err1)
			} else {
				if _, err2 := f.WriteString(fmt.Sprintf("%s\n", yaml)); err2 != nil {
					multiErr = multierror.Append(multiErr, err2)
				}
			}
		}
	}
	return multiErr
}

// WaitForDeploymentsReady wait up to 'timeout' duration
// return an error if deployments are not ready
func WaitForDeploymentsReady(ns string, timeout time.Duration) error {
	retry := Retrier{
		BaseDelay:   10 * time.Second,
		MaxDelay:    10 * time.Second,
		MaxDuration: timeout,
		Retries:     20,
	}

	_, err := retry.Retry(context.Background(), func(_ context.Context, _ int) error {
		nr, err := CheckDeploymentsReady(ns)
		if err != nil {
			return &Break{err}
		}

		if nr == 0 { // done
			return nil
		}
		return fmt.Errorf("%d deployments not ready", nr)
	})
	return err
}

// CheckDeploymentsReady checks if deployment resources are ready.
// get podsReady() sometimes gets pods created by the "Job" resource which never reach the "Running" steady state.
func CheckDeploymentsReady(ns string) (int, error) {
	CMD := "kubectl -n %s get deployments -ao jsonpath='{range .items[*]}{@.metadata.name}{\" \"}" +
		"{@.status.availableReplicas}{\"\\n\"}{end}'"
	out, err := Shell(fmt.Sprintf(CMD, ns))

	if err != nil {
		return 0, fmt.Errorf("could not list deployments in namespace %q: %v", ns, err)
	}

	notReady := 0
	for _, line := range strings.Split(out, "\n") {
		flds := strings.Fields(line)
		if len(flds) < 2 {
			continue
		}
		if flds[1] == "0" { // no replicas ready
			notReady++
		}
	}

	if notReady == 0 {
		log.Infof("All deployments are ready")
	}
	return notReady, nil
}
