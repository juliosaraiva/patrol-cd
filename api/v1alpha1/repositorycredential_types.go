package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RepositoryCredential is a credential template applied to every repository whose URL starts with
// spec.urlPrefix and that has no credentials of its own (longest matching prefix wins). It replaces
// Argo CD's Secrets labeled argocd.argoproj.io/secret-type=repo-creds (and repo-write-creds).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=repositorycredentials,singular=repositorycredential,scope=Namespaced,shortName=prepocred,categories=patrol
// +kubebuilder:printcolumn:name="URL Prefix",type=string,JSONPath=`.spec.urlPrefix`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Purpose",type=string,JSONPath=`.spec.purpose`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type RepositoryCredential struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the credential template.
	Spec RepositoryCredentialSpec `json:"spec"`
	// Status is the observed state of the template.
	// +optional
	Status RepositoryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RepositoryCredentialList is a list of RepositoryCredentials.
type RepositoryCredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RepositoryCredential `json:"items"`
}

func init() {
	register(&RepositoryCredential{}, &RepositoryCredentialList{})
}

// RepositoryCredentialSpec is a credential template.
// +kubebuilder:validation:XValidation:rule="!has(self.purpose) || self.purpose != 'write' || !has(self.type) || self.type == 'git'",message="purpose write is only supported for git repositories"
type RepositoryCredentialSpec struct {
	// URLPrefix selects the repositories the template applies to.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	URLPrefix string `json:"urlPrefix"`
	// Type of the matching repositories.
	// +optional
	// +kubebuilder:default=git
	Type RepositoryType `json:"type,omitempty"`
	// Purpose of the credentials.
	// +optional
	// +kubebuilder:default=read
	Purpose RepositoryPurpose `json:"purpose,omitempty"`

	RepositoryAccess `json:",inline"`
}
