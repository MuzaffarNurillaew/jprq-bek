/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Phases reported in status.phase. A derived convenience for humans and printer
// columns; conditions are the machine-readable truth (DESIGN.md §5.5).
const (
	PhasePending  = "Pending"
	PhaseActive   = "Active"
	PhaseDegraded = "Degraded"
	PhaseFailed   = "Failed"
)

// Condition types. Three independent causes of not-working, each separately
// observable, so Ready=False is never the whole story (DESIGN.md §5.5).
const (
	ConditionReady        = "Ready"
	ConditionBackendReady = "BackendReady"
	ConditionTokenReady   = "TokenReady"
)

// Condition reasons. Declared here rather than inline so the reconciler and its
// tests cannot drift on string literals.
const (
	ReasonSecretNotFound    = "SecretNotFound"
	ReasonSecretKeyNotFound = "SecretKeyNotFound"
	ReasonEmptyToken        = "EmptyToken"
	ReasonTokenFound        = "TokenFound"

	ReasonServiceNotFound   = "ServiceNotFound"
	ReasonPortNotResolvable = "PortNotResolvable"
	ReasonNoReadyEndpoints  = "NoReadyEndpoints"
	ReasonEndpointsReady    = "EndpointsReady"

	ReasonTunnelActive = "TunnelActive"
	ReasonPodNotReady  = "PodNotReady"
	ReasonPodPending   = "PodPending"

	// Terminal verdicts from the jprq server, keyed off agent exit codes
	// (DESIGN.md §7.2, cli/jprq/exitcodes.go).
	ReasonSubdomainBusy             = "SubdomainBusy"
	ReasonCNAMEBusy                 = "CNAMEBusy"
	ReasonAccountTunnelLimitReached = "AccountTunnelLimitReached"
	ReasonAuthFailed                = "AuthFailed"
	ReasonNotAllowlisted            = "NotAllowlisted"
	ReasonInvalidSubdomain          = "InvalidSubdomain"

	ReasonAgentExited = "AgentExited"
)

// Collision behaviors for spec.subdomainCollisionBehavior (DESIGN.md §9.3).
const (
	CollisionFail      = "fail"
	CollisionRetry     = "retry"
	CollisionAddSuffix = "addSuffix"
)

// JprqTunnelSpec defines the desired state of JprqTunnel
//
// +kubebuilder:validation:XValidation:rule="self.subdomain != 'www' && self.subdomain != 'jprq'",message="subdomains 'www' and 'jprq' are reserved by the jprq server"
// +kubebuilder:validation:XValidation:rule="!has(self.subdomainCollisionBehavior) || self.subdomainCollisionBehavior != 'addSuffix'",message="subdomainCollisionBehavior 'addSuffix' is not implemented in v1alpha1"
// +kubebuilder:validation:XValidation:rule="!has(self.cname)",message="spec.cname is not implemented in v1alpha1"
type JprqTunnelSpec struct {
	// subdomain is the public name to claim, reachable at
	// https://<subdomain>.<jprq domain>.
	//
	// The bounds and pattern are the jprq server's own rules transcribed, so
	// every subdomain this API accepts is one the server will accept, making
	// "invalid subdomain" structurally impossible at runtime rather than a
	// crash-loop to diagnose (DESIGN.md §5.2).
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=38
	// +kubebuilder:validation:Pattern=`^[a-z0-9](?:[a-z0-9]|-[a-z0-9]){2,37}$`
	// +required
	Subdomain string `json:"subdomain"`

	// subdomainCollisionBehavior decides what happens when the jprq server
	// reports the subdomain is already taken. Collisions are only discoverable
	// by trying, since jprq exposes no list-tunnels API (DESIGN.md §9.3).
	//
	// "fail" records a terminal failure and does not retry. "retry" leaves the
	// agent to keep re-claiming until the holder releases the name.
	// "addSuffix" is schema-visible but rejected in v1alpha1.
	// +kubebuilder:validation:Enum=fail;retry;addSuffix
	// +kubebuilder:default=fail
	// +optional
	SubdomainCollisionBehavior string `json:"subdomainCollisionBehavior,omitempty"`

	// backend is the in-cluster Service to expose.
	// +required
	Backend BackendRef `json:"backend"`

	// jprqTokenSecret names the Secret holding the jprq auth token.
	// +required
	JprqTokenSecret SecretKeyRef `json:"jprqTokenSecret"`

	// cname is reserved for custom hostnames. Schema only in v1alpha1, and
	// rejected at admission (DESIGN.md §11).
	// +optional
	CNAME *CNAMESpec `json:"cname,omitempty"`
}

// BackendRef identifies the Service that receives tunneled traffic. Any
// namespace is allowed: ClusterIP DNS is cluster-wide, so cross-namespace
// reachability is a non-issue (DESIGN.md §5.1).
type BackendRef struct {
	// namespace of the backend Service.
	// +kubebuilder:validation:MinLength=1
	// +required
	Namespace string `json:"namespace"`

	// serviceName of the backend Service.
	// +kubebuilder:validation:MinLength=1
	// +required
	ServiceName string `json:"serviceName"`

	// port is a Service port name or number. Names are resolved to a number by
	// the controller, so the agent only ever sees a number and therefore needs
	// no API access of its own (DESIGN.md §9.2).
	// +required
	Port intstr.IntOrString `json:"port"`
}

// SecretKeyRef points at one key of a Secret in the operator namespace.
type SecretKeyRef struct {
	// name of the Secret, which must live in the operator namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// key within the Secret holding the raw token.
	// +kubebuilder:default=authToken
	// +optional
	Key string `json:"key,omitempty"`
}

// CNAMESpec is the deferred custom-hostname configuration (DESIGN.md §11).
type CNAMESpec struct {
	// hostname to serve, e.g. tunnel.example.com.
	// +required
	Hostname string `json:"hostname"`

	// cloudflareTokenSecret names the Secret holding a Cloudflare API token.
	// +required
	CloudflareTokenSecret SecretKeyRef `json:"cloudflareTokenSecret"`
}

// JprqTunnelStatus defines the observed state of JprqTunnel.
type JprqTunnelStatus struct {
	// phase is a derived summary for humans and printer columns: Pending,
	// Active, Degraded or Failed. Conditions are the machine-readable truth.
	//
	// Failed is sticky: it clears only when metadata.generation changes, so
	// re-applying unchanged YAML does not re-arm a failed tunnel. This is what
	// stops a rejected token from crash-looping forever (DESIGN.md §5.5, §10.1).
	// +optional
	Phase string `json:"phase,omitempty"`

	// url is the public address of the tunnel.
	// +optional
	URL string `json:"url,omitempty"`

	// assignedSubdomain is the subdomain actually claimed. Equal to
	// spec.subdomain in v1alpha1; kept distinct so addSuffix can pin a
	// generated name here later without a status migration (DESIGN.md §5.5).
	// +optional
	AssignedSubdomain string `json:"assignedSubdomain,omitempty"`

	// backendAddress is the resolved host:port the agent was given, with the
	// Service port name already resolved to a number. Answers "what did the
	// agent actually dial?" and powers the BACKEND printer column.
	// +optional
	BackendAddress string `json:"backendAddress,omitempty"`

	// podName is the agent Pod in the operator namespace.
	// +optional
	PodName string `json:"podName,omitempty"`

	// lastExitCode is the agent's most recent container exit code, which is how
	// server verdicts are classified (DESIGN.md §7.2).
	// +optional
	LastExitCode *int32 `json:"lastExitCode,omitempty"`

	// lastFailureMessage is the most recent human-readable failure.
	// +optional
	LastFailureMessage string `json:"lastFailureMessage,omitempty"`

	// restartCount mirrors kubelet's container restart count. Since the agent
	// exits whenever the event stream drops, restartPolicy: Always is the
	// reconnect mechanism and this is a truthful health signal (DESIGN.md §6.2).
	// +optional
	RestartCount int32 `json:"restartCount,omitempty"`

	// observedGeneration is the spec generation this status reflects. Also the
	// gate on sticky terminal failures.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the JprqTunnel resource.
	//
	// Condition types are Ready, BackendReady and TokenReady: three independent
	// causes of not-working, each separately observable.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Subdomain",type=string,JSONPath=".spec.subdomain"
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=".status.url"
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=".status.backendAddress"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// JprqTunnel is the Schema for the jprqtunnels API
type JprqTunnel struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of JprqTunnel
	// +required
	Spec JprqTunnelSpec `json:"spec"`

	// status defines the observed state of JprqTunnel
	// +optional
	Status JprqTunnelStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// JprqTunnelList contains a list of JprqTunnel
type JprqTunnelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []JprqTunnel `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &JprqTunnel{}, &JprqTunnelList{})
		return nil
	})
}
