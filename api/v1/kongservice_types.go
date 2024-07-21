package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KongServiceSpec defines the desired state of KongService
type KongServiceSpec struct {
	Name   string  `json:"name"`
	URL    string  `json:"url"`
	Routes []Route `json:"routes"`
}

// Route defines a Kong route
type Route struct {
	Path        string   `json:"path"`
	Methods     []string `json:"methods,omitempty"`
	Hosts       []string `json:"hosts,omitempty"`
	StripPath   *bool    `json:"stripPath,omitempty"`
	PreserveHost *bool   `json:"preserveHost,omitempty"`
	Protocols   []string `json:"protocols,omitempty"`
	Tags         []string `json:"tags,omitempty"`
}
// KongServiceStatus defines the observed state of KongService
type KongServiceStatus struct {
	ServiceID string            `json:"serviceId,omitempty"`
	RouteIDs  map[string]string `json:"routeIds,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// KongService is the Schema for the kongservices API
type KongService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KongServiceSpec   `json:"spec,omitempty"`
	Status KongServiceStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// KongServiceList contains a list of KongService
type KongServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KongService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KongService{}, &KongServiceList{})
}
