package settings

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testNamespace = "patrol"

// testServerKey is a 32-byte test signing key (not a real secret).
const testServerKey = "0123456789abcdef0123456789abcdef"

func partOfLabels() map[string]string { return map[string]string{LabelKeyPartOf: LabelValuePartOf} }

func newCM(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: partOfLabels(), ResourceVersion: "1"},
		Data:       data,
	}
}

func newSecret(name string, data map[string]string, labelled bool) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, ResourceVersion: "1"},
		Data:       map[string][]byte{},
	}
	if labelled {
		s.Labels = partOfLabels()
	}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func baseSecretData() map[string]string {
	return map[string]string{SecretKeyServerSecretKey: testServerKey}
}

// in builds an Input with patrol-cm (data), a valid patrol-secret and extra objects.
func in(cmData map[string]string, extra ...any) Input {
	input := Input{Namespace: testNamespace}
	if cmData != nil {
		input.ConfigMaps = append(input.ConfigMaps, newCM(DefaultConfigMapName, cmData))
	}
	hasSecret := false
	for _, o := range extra {
		switch v := o.(type) {
		case *corev1.ConfigMap:
			input.ConfigMaps = append(input.ConfigMaps, v)
		case *corev1.Secret:
			if v.Name == DefaultSecretName {
				hasSecret = true
			}
			input.Secrets = append(input.Secrets, v)
		}
	}
	if !hasSecret {
		input.Secrets = append(input.Secrets, newSecret(DefaultSecretName, baseSecretData(), true))
	}
	return input
}

// issuesFor returns the issues of key in object.
func issuesFor(s *Settings, object, key string) []Issue {
	var out []Issue
	for _, is := range s.Validate() {
		if is.Object == object && is.Key == key {
			out = append(out, is)
		}
	}
	return out
}

// requireIssue asserts an issue of key in object with severity whose message contains want.
func requireIssue(t *testing.T, s *Settings, object, key string, sev Severity, want string) {
	t.Helper()
	for _, is := range issuesFor(s, object, key) {
		if is.Severity == sev && strings.Contains(is.Message, want) {
			return
		}
	}
	t.Fatalf("no %s issue for %s key %q containing %q; issues:\n%s", sev, object, key, want, dumpIssues(s.Validate()))
}

// requireNoIssues asserts that object has no issues at all.
func requireNoIssues(t *testing.T, s *Settings, object string) {
	t.Helper()
	var got []Issue
	for _, is := range s.Validate() {
		if is.Object == object {
			got = append(got, is)
		}
	}
	if len(got) > 0 {
		t.Fatalf("unexpected issues for %s:\n%s", object, dumpIssues(got))
	}
}

func dumpIssues(issues []Issue) string {
	var b strings.Builder
	for _, is := range issues {
		b.WriteString("  ")
		b.WriteString(is.String())
		b.WriteByte('\n')
	}
	return b.String()
}
