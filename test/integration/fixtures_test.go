package integration

import (
	"fmt"
	"strings"
)

// Manifests of the sample applications. Kinds are chosen so envtest (no kubelet, no
// controller-manager) can report health: ConfigMaps, Services and ServiceAccounts are healthy
// as soon as they exist; Jobs and Deployments are completed by Stack.SimulateWorkloads or by
// the test (stack.CompleteJob, stack.RolloutDeployment).

// guestbookFiles is the plain-YAML guestbook: a PreSync migration Job (hook, generateName),
// a ConfigMap, a Service and a Deployment.
func guestbookFiles(greeting string) map[string]string {
	return map[string]string{
		"guestbook/00-presync-job.yaml": `apiVersion: batch/v1
kind: Job
metadata:
  generateName: db-migrate-
  annotations:
    cd.patrol.example/hook: PreSync
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: migrate
          image: registry.k8s.io/e2e-test-images/busybox:1.36.1-1
          command: ["sh", "-c", "echo migrated"]
`,
		"guestbook/configmap.yaml": fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: guestbook-config
  labels:
    app: guestbook
data:
  GREETING: %q
`, greeting),
		"guestbook/service.yaml": `apiVersion: v1
kind: Service
metadata:
  name: guestbook-ui
  labels:
    app: guestbook
spec:
  ports:
    - port: 80
      targetPort: 8080
  selector:
    app: guestbook-ui
`,
		"guestbook/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: guestbook-ui
  labels:
    app: guestbook
spec:
  replicas: 1
  revisionHistoryLimit: 3
  selector:
    matchLabels:
      app: guestbook-ui
  template:
    metadata:
      labels:
        app: guestbook-ui
    spec:
      containers:
        - name: guestbook-ui
          image: registry.k8s.io/e2e-test-images/agnhost:2.39
          ports:
            - containerPort: 8080
`,
	}
}

// configMap renders a ConfigMap manifest with optional annotations.
func configMap(name string, data map[string]string, annotations map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  labels:\n    app: sample\n", name)
	if len(annotations) > 0 {
		b.WriteString("  annotations:\n")
		for _, k := range sortedKeys(annotations) {
			fmt.Fprintf(&b, "    %s: %q\n", k, annotations[k])
		}
	}
	if len(data) > 0 {
		b.WriteString("data:\n")
		for _, k := range sortedKeys(data) {
			fmt.Fprintf(&b, "  %s: %q\n", k, data[k])
		}
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
