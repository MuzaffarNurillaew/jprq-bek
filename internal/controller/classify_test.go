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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	tunnelv1alpha1 "github.com/muzaffarnurillaew/jprq-bek/api/v1alpha1"
	"github.com/muzaffarnurillaew/jprq-bek/cli/jprq"
)

const (
	testTokenSecretName = "jprq-token"
	testBackendHostFQDN = "web.default.svc"
	testSubdomainWeb    = "muzaffar-web"
	testServiceNameWeb  = "web"
)

// testScheme is the minimum scheme desiredPod needs to resolve the owner's GVK.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := tunnelv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding tunnel.jprq.io to scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding core/v1 to scheme: %v", err)
	}
	return s
}

func TestClassifyExit(t *testing.T) {
	tests := []struct {
		name         string
		exitCode     int32
		behavior     string
		terminal     bool
		deletePod    bool
		tokenInvalid bool
		reason       string
	}{
		{
			name:     "auth failure is terminal and invalidates the token",
			exitCode: jprq.ExitAuthFailed, behavior: tunnelv1alpha1.CollisionFail,
			terminal: true, deletePod: true, tokenInvalid: true,
			reason: tunnelv1alpha1.ReasonAuthFailed,
		},
		{
			name:     "not allowlisted is terminal and invalidates the token",
			exitCode: jprq.ExitNotAllowlisted, behavior: tunnelv1alpha1.CollisionFail,
			terminal: true, deletePod: true, tokenInvalid: true,
			reason: tunnelv1alpha1.ReasonNotAllowlisted,
		},
		{
			name:     "tunnel limit is terminal but the token is fine",
			exitCode: jprq.ExitAccountTunnelLimit, behavior: tunnelv1alpha1.CollisionFail,
			terminal: true, deletePod: true,
			reason: tunnelv1alpha1.ReasonAccountTunnelLimitReached,
		},
		{
			name:     "subdomain busy with fail is terminal",
			exitCode: jprq.ExitSubdomainBusy, behavior: tunnelv1alpha1.CollisionFail,
			terminal: true, deletePod: true,
			reason: tunnelv1alpha1.ReasonSubdomainBusy,
		},
		{
			// The whole point of decision 2: kubelet owns the retry, so the
			// controller must neither mark this terminal nor delete the pod.
			name:     "subdomain busy with retry is not terminal and keeps the pod",
			exitCode: jprq.ExitSubdomainBusy, behavior: tunnelv1alpha1.CollisionRetry,
			terminal: false, deletePod: false,
			reason: tunnelv1alpha1.ReasonSubdomainBusy,
		},
		{
			name:     "an empty collision behavior defaults to fail semantics",
			exitCode: jprq.ExitSubdomainBusy, behavior: "",
			terminal: true, deletePod: true,
			reason: tunnelv1alpha1.ReasonSubdomainBusy,
		},
		{
			name:     "invalid subdomain is terminal",
			exitCode: jprq.ExitInvalidSubdomain, behavior: tunnelv1alpha1.CollisionFail,
			terminal: true, deletePod: true,
			reason: tunnelv1alpha1.ReasonInvalidSubdomain,
		},
		{
			name:     "cname busy is terminal",
			exitCode: jprq.ExitCNAMEBusy, behavior: tunnelv1alpha1.CollisionFail,
			terminal: true, deletePod: true,
			reason: tunnelv1alpha1.ReasonCNAMEBusy,
		},
		{
			// The expected steady-state failure: upstream has no reconnect, so
			// the agent exits and kubelet restarts it.
			name:     "a dropped event stream is retryable",
			exitCode: jprq.ExitEventStreamDropped, behavior: tunnelv1alpha1.CollisionFail,
			terminal: false, deletePod: false,
			reason: tunnelv1alpha1.ReasonAgentExited,
		},
		{
			name:     "an unreachable event server is retryable",
			exitCode: jprq.ExitEventServerUnreachable, behavior: tunnelv1alpha1.CollisionFail,
			terminal: false, deletePod: false,
			reason: tunnelv1alpha1.ReasonAgentExited,
		},
		{
			name:     "a config failure is retryable",
			exitCode: jprq.ExitConfigFailure, behavior: tunnelv1alpha1.CollisionFail,
			terminal: false, deletePod: false,
			reason: tunnelv1alpha1.ReasonAgentExited,
		},
		{
			name:     "an unclassified exit is retryable",
			exitCode: jprq.ExitUnexpected, behavior: tunnelv1alpha1.CollisionFail,
			terminal: false, deletePod: false,
			reason: tunnelv1alpha1.ReasonAgentExited,
		},
		{
			name:     "a clean exit is retryable",
			exitCode: jprq.ExitOK, behavior: tunnelv1alpha1.CollisionFail,
			terminal: false, deletePod: false,
			reason: tunnelv1alpha1.ReasonAgentExited,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyExit(tt.exitCode, tt.behavior)

			if got.Terminal != tt.terminal {
				t.Errorf("Terminal = %v, want %v", got.Terminal, tt.terminal)
			}
			if got.DeletePod != tt.deletePod {
				t.Errorf("DeletePod = %v, want %v", got.DeletePod, tt.deletePod)
			}
			if got.TokenInvalid != tt.tokenInvalid {
				t.Errorf("TokenInvalid = %v, want %v", got.TokenInvalid, tt.tokenInvalid)
			}
			if got.Reason != tt.reason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.reason)
			}
			if got.Message == "" {
				t.Error("Message is empty; status would explain nothing to the user")
			}
			// Deleting a pod for a failure that will be retried would race
			// kubelet's own restart.
			if got.DeletePod && !got.Terminal {
				t.Error("DeletePod set without Terminal: that would race kubelet's restart")
			}
		})
	}
}

func TestClassifyPod(t *testing.T) {
	agentRunning := func(ready bool) corev1.ContainerStatus {
		return corev1.ContainerStatus{
			Name:  agentContainerName,
			Ready: ready,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}
	}

	tests := []struct {
		name     string
		statuses []corev1.ContainerStatus
		wantOK   bool
		wantCode int32
	}{
		{
			name:   "no container status yet",
			wantOK: false,
		},
		{
			name: "a different container's status is ignored",
			statuses: []corev1.ContainerStatus{{
				Name: "sidecar",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: jprq.ExitAuthFailed,
				}},
			}},
			wantOK: false,
		},
		{
			name:     "running but not yet ready and never terminated",
			statuses: []corev1.ContainerStatus{agentRunning(false)},
			wantOK:   false,
		},
		{
			// The guard that matters. With restartPolicy: Always,
			// LastTerminationState survives a successful restart, so a healthy
			// container must never be judged by an old termination or a working
			// tunnel would be marked Failed.
			name: "running and ready ignores a stale termination record",
			statuses: func() []corev1.ContainerStatus {
				cs := agentRunning(true)
				cs.RestartCount = 3
				cs.LastTerminationState = corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: jprq.ExitAuthFailed},
				}
				return []corev1.ContainerStatus{cs}
			}(),
			wantOK: false,
		},
		{
			name: "crash-looping reads the exit code from LastTerminationState",
			statuses: []corev1.ContainerStatus{{
				Name:  agentContainerName,
				Ready: false,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff",
				}},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: jprq.ExitAccountTunnelLimit},
				},
				RestartCount: 2,
			}},
			wantOK:   true,
			wantCode: jprq.ExitAccountTunnelLimit,
		},
		{
			name: "a currently terminated container is classified too",
			statuses: []corev1.ContainerStatus{{
				Name: agentContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: jprq.ExitSubdomainBusy,
				}},
			}},
			wantOK:   true,
			wantCode: jprq.ExitSubdomainBusy,
		},
		{
			name: "running but unready with a previous termination is classified",
			statuses: func() []corev1.ContainerStatus {
				cs := agentRunning(false)
				cs.LastTerminationState = corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: jprq.ExitEventStreamDropped},
				}
				return []corev1.ContainerStatus{cs}
			}(),
			wantOK:   true,
			wantCode: jprq.ExitEventStreamDropped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: tt.statuses}}

			_, code, ok := classifyPod(pod, tunnelv1alpha1.CollisionFail)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
		})
	}
}

func TestSpecHashChangesWithEveryInput(t *testing.T) {
	base := agentPodConfig{
		Image:             "example.test/agent:v1",
		OperatorNamespace: testOperatorNamespace,
		BackendHost:       testBackendHostFQDN,
		BackendPort:       8080,
		Subdomain:         testSubdomainWeb,
		SecretName:        testTokenSecretName,
		SecretKey:         defaultTokenKey,
		TokenDigest:       tokenDigest([]byte("token-a")),
	}

	first, second := base.specHash(), base.specHash()
	if first != second {
		t.Fatal("specHash is not stable across calls")
	}

	mutations := map[string]func(*agentPodConfig){
		"image":       func(c *agentPodConfig) { c.Image = "example.test/agent:v2" },
		"backendHost": func(c *agentPodConfig) { c.BackendHost = "other.default.svc" },
		"backendPort": func(c *agentPodConfig) { c.BackendPort = 9090 },
		"subdomain":   func(c *agentPodConfig) { c.Subdomain = "muzaffar-api" },
		"secretName":  func(c *agentPodConfig) { c.SecretName = "other-token" },
		"secretKey":   func(c *agentPodConfig) { c.SecretKey = "token" },
		// The addition to §9.1: hashing the token *contents* is what makes an
		// in-place rotation actually recreate the pod. Without this the agent
		// would keep running with the token it read at startup.
		"tokenDigest": func(c *agentPodConfig) { c.TokenDigest = tokenDigest([]byte("token-b")) },
	}

	for field, mutate := range mutations {
		t.Run(field, func(t *testing.T) {
			mutated := base
			mutate(&mutated)
			if mutated.specHash() == base.specHash() {
				t.Errorf("specHash ignores %s, so a change to it would never recreate the pod", field)
			}
		})
	}

	// The namespace is not a hash input: it cannot change without the pod being
	// a different object entirely.
	moved := base
	moved.OperatorNamespace = "elsewhere"
	if moved.specHash() != base.specHash() {
		t.Error("specHash should not depend on the operator namespace")
	}
}

func TestDesiredPodShape(t *testing.T) {
	tunnel := &tunnelv1alpha1.JprqTunnel{}
	tunnel.Name = testServiceNameWeb

	cfg := agentPodConfig{
		Image:             "example.test/agent:v1",
		OperatorNamespace: testOperatorNamespace,
		BackendHost:       testBackendHostFQDN,
		BackendPort:       8080,
		Subdomain:         testSubdomainWeb,
		SecretName:        testTokenSecretName,
		SecretKey:         defaultTokenKey,
		TokenDigest:       tokenDigest([]byte("token")),
	}

	pod, err := desiredPod(tunnel, cfg, testScheme(t))
	if err != nil {
		t.Fatalf("desiredPod: %v", err)
	}

	if pod.Name != "jprq-web" || pod.Namespace != testOperatorNamespace {
		t.Errorf("pod is %s/%s, want %s/jprq-web", pod.Namespace, pod.Name, testOperatorNamespace)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("restartPolicy = %q; Always is the reconnect mechanism", pod.Spec.RestartPolicy)
	}
	if got := pod.Spec.AutomountServiceAccountToken; got == nil || *got {
		t.Error("automountServiceAccountToken must be false: the agent never calls the API server")
	}

	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	want := map[string]string{
		"JPRQ_PROTOCOL":     "http",
		"JPRQ_BACKEND_HOST": testBackendHostFQDN,
		"JPRQ_BACKEND_PORT": "8080",
		"JPRQ_SUBDOMAIN":    testSubdomainWeb,
		"JPRQ_TOKEN_FILE":   "/etc/jprq/authToken",
		"JPRQ_HEALTH_ADDR":  ":9000",
	}
	for name, value := range want {
		if env[name] != value {
			t.Errorf("env %s = %q, want %q", name, env[name], value)
		}
	}

	probe := pod.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.HTTPGet == nil {
		t.Fatal("readiness probe must be an httpGet")
	}
	// kubelet probes from the node, outside the pod's network namespace, so a
	// Host of 127.0.0.1 could never be reached (DESIGN.md §7.3).
	if probe.HTTPGet.Host != "" {
		t.Errorf("probe Host = %q, must be empty so kubelet can reach it", probe.HTTPGet.Host)
	}

	if len(pod.OwnerReferences) != 1 {
		t.Fatalf("want exactly one owner reference, got %d", len(pod.OwnerReferences))
	}
	owner := pod.OwnerReferences[0]
	if owner.Kind != "JprqTunnel" || owner.Name != testServiceNameWeb {
		t.Errorf("owner = %s/%s, want JprqTunnel/web", owner.Kind, owner.Name)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Error("owner reference must be a controller reference for Owns() to match it")
	}

	if pod.Annotations[SpecHashAnnotation] != cfg.specHash() {
		t.Error("pod is missing its spec-hash annotation")
	}
	if pod.Labels[TunnelLabel] != testServiceNameWeb {
		t.Errorf("pod label %s = %q, want web", TunnelLabel, pod.Labels[TunnelLabel])
	}

	if mode := pod.Spec.Volumes[0].Secret.DefaultMode; mode == nil || *mode != 0o400 {
		t.Errorf("token volume mode = %v, want 0400", mode)
	}
	if got := pod.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem; got == nil || !*got {
		t.Error("readOnlyRootFilesystem must be true")
	}
	if want := new(int64(5)); pod.Spec.TerminationGracePeriodSeconds == nil ||
		*pod.Spec.TerminationGracePeriodSeconds != *want {
		t.Error("terminationGracePeriodSeconds must be 5")
	}
}
