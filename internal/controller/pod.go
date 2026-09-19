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

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	tunnelv1alpha1 "github.com/muzaffarnurillaew/jprq-bek/api/v1alpha1"
)

const (
	// SpecHashAnnotation carries the hash of everything that affects the pod.
	// A mismatch means delete-and-recreate, never patch: a Pod's env is
	// immutable (DESIGN.md §9.1).
	SpecHashAnnotation = "jprq.io/spec-hash"

	// TunnelLabel maps an agent pod back to its JprqTunnel.
	TunnelLabel = "jprq.io/tunnel"

	agentContainerName = "agent"
	agentAppName       = "jprq-agent"

	tokenVolumeName = "token"
	tokenMountPath  = "/etc/jprq"

	// healthPort and healthAddr must agree. The address deliberately binds all
	// interfaces rather than 127.0.0.1: kubelet probes from the node, outside
	// the pod's network namespace, so a loopback-bound listener could never be
	// reached (DESIGN.md §7.3).
	healthPort = 9000
	healthAddr = ":9000"

	// protocolHTTP is the only protocol v1 drives. The CLI still supports tcp;
	// enforcing HTTP-only is the controller's job, not the CLI's (§17.4).
	protocolHTTP = "http"
)

// agentPodConfig is the fully resolved input to the agent pod. Everything here
// is already concrete: the port is a number, not a Service port name, because
// the agent has no API access to resolve one (DESIGN.md §9.2).
type agentPodConfig struct {
	Image             string
	OperatorNamespace string
	BackendHost       string
	BackendPort       int32
	Subdomain         string
	SecretName        string
	SecretKey         string

	// TokenDigest is a hash of the token bytes, included in the spec hash so an
	// in-place token rotation actually recreates the pod. Without it the agent
	// would keep running with the token it read at startup.
	TokenDigest string
}

// podName is deterministic so reconciliation is idempotent. JprqTunnel is
// cluster-scoped, so its name is already unique cluster-wide.
func podName(tunnelName string) string {
	return "jprq-" + tunnelName
}

// specHash covers every input that affects the pod. Field order is the struct's
// declaration order, so encoding/json output is stable.
func (c agentPodConfig) specHash() string {
	payload := struct {
		Image       string `json:"image"`
		Protocol    string `json:"protocol"`
		Subdomain   string `json:"subdomain"`
		BackendHost string `json:"backendHost"`
		BackendPort int32  `json:"backendPort"`
		SecretName  string `json:"secretName"`
		SecretKey   string `json:"secretKey"`
		TokenDigest string `json:"tokenDigest"`
		HealthAddr  string `json:"healthAddr"`
	}{
		Image:       c.Image,
		Protocol:    protocolHTTP,
		Subdomain:   c.Subdomain,
		BackendHost: c.BackendHost,
		BackendPort: c.BackendPort,
		SecretName:  c.SecretName,
		SecretKey:   c.SecretKey,
		TokenDigest: c.TokenDigest,
		HealthAddr:  healthAddr,
	}

	// Marshalling a flat struct of scalars cannot fail.
	encoded, _ := json.Marshal(payload) //nolint:errchkjson // flat scalar struct
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])[:16]
}

// tokenDigest hashes raw token bytes for inclusion in the spec hash. Only a
// digest of a high-entropy token ever reaches the pod annotation, the same
// pattern as Helm's checksum/secret.
func tokenDigest(token []byte) string {
	sum := sha256.Sum256(token)
	return hex.EncodeToString(sum[:])
}

// desiredPod builds the agent pod per DESIGN.md §6.3.
//
// The owner reference is set here rather than by the caller so it can never be
// forgotten. A namespaced Pod may legally declare a cluster-scoped owner, which
// is what makes garbage collection work despite the CR having no namespace
// (DESIGN.md §5.1).
func desiredPod(
	tunnel *tunnelv1alpha1.JprqTunnel,
	cfg agentPodConfig,
	scheme *runtime.Scheme,
) (*corev1.Pod, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName(tunnel.Name),
			Namespace: cfg.OperatorNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": agentAppName,
				TunnelLabel:              tunnel.Name,
			},
			Annotations: map[string]string{
				SpecHashAnnotation: cfg.specHash(),
			},
		},
		Spec: corev1.PodSpec{
			// The agent exits whenever the event stream drops (upstream has no
			// reconnect), so Always *is* the reconnect mechanism, and kubelet's
			// backoff is the only retry engine (DESIGN.md §6.2).
			RestartPolicy: corev1.RestartPolicyAlways,
			// Our agent handles SIGTERM and exits promptly.
			TerminationGracePeriodSeconds: new(int64(5)),
			// The agent never calls the API server; the controller resolved
			// everything for it (DESIGN.md §9.2).
			AutomountServiceAccountToken: new(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: new(true),
				RunAsUser:    new(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:  agentContainerName,
				Image: cfg.Image,
				Env: []corev1.EnvVar{
					{Name: "JPRQ_PROTOCOL", Value: protocolHTTP},
					{Name: "JPRQ_BACKEND_HOST", Value: cfg.BackendHost},
					{Name: "JPRQ_BACKEND_PORT", Value: fmt.Sprintf("%d", cfg.BackendPort)},
					{Name: "JPRQ_SUBDOMAIN", Value: cfg.Subdomain},
					{Name: "JPRQ_TOKEN_FILE", Value: tokenMountPath + "/" + cfg.SecretKey},
					{Name: "JPRQ_HEALTH_ADDR", Value: healthAddr},
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      tokenVolumeName,
					MountPath: tokenMountPath,
					ReadOnly:  true,
				}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/healthz",
							Port: intstr.FromInt32(healthPort),
							// No Host: kubelet probes from the node, so
							// 127.0.0.1 would target the node's loopback.
						},
					},
					PeriodSeconds: 10,
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: new(false),
					ReadOnlyRootFilesystem:   new(true),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: tokenVolumeName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: cfg.SecretName,
						Items: []corev1.KeyToPath{{
							Key:  cfg.SecretKey,
							Path: cfg.SecretKey,
						}},
						DefaultMode: new(int32(0o400)),
					},
				},
			}},
		},
	}

	if err := controllerutil.SetControllerReference(tunnel, pod, scheme); err != nil {
		return nil, fmt.Errorf("setting owner reference: %w", err)
	}
	return pod, nil
}
