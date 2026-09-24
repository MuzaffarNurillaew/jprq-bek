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
	"fmt"

	corev1 "k8s.io/api/core/v1"

	tunnelv1alpha1 "github.com/muzaffarnurillaew/jprq-bek/api/v1alpha1"
	"github.com/muzaffarnurillaew/jprq-bek/cli/jprq"
)

// verdict is what the controller concludes from an agent container exit.
type verdict struct {
	// Terminal means the jprq server issued a judgement retrying cannot change,
	// so the tunnel goes Failed and stays there until metadata.generation
	// changes (DESIGN.md §5.5).
	Terminal bool

	// DeletePod stops an otherwise endless crash-loop against a verdict that
	// will not change. Only ever set together with Terminal: retryable failures
	// are left to kubelet's restart backoff, which is the single retry engine.
	DeletePod bool

	// TokenInvalid additionally flips TokenReady=False, because the server
	// rejected the credential rather than the request.
	TokenInvalid bool

	Reason  string
	Message string
}

// classifyExit maps an agent exit code to a verdict.
//
// Codes come from cli/jprq/exitcodes.go, which the agent and controller share
// deliberately — cli/jprq is not under internal/ so this import is legal, and a
// shared constant block is what keeps the two from drifting (DESIGN.md §17.1).
//
// Retryable codes return Terminal=false and DeletePod=false: with
// restartPolicy: Always a container restart is itself the retry, so the
// controller must not also delete the pod or the two would race (§6.2).
func classifyExit(exitCode int32, collisionBehavior string) verdict {
	switch exitCode {
	case jprq.ExitAuthFailed:
		return verdict{
			Terminal: true, DeletePod: true, TokenInvalid: true,
			Reason:  tunnelv1alpha1.ReasonAuthFailed,
			Message: "jprq rejected the auth token; check the token Secret and re-create this tunnel",
		}

	case jprq.ExitNotAllowlisted:
		return verdict{
			Terminal: true, DeletePod: true, TokenInvalid: true,
			Reason:  tunnelv1alpha1.ReasonNotAllowlisted,
			Message: "this jprq account is not allowlisted for the invite-only service",
		}

	case jprq.ExitAccountTunnelLimit:
		return verdict{
			Terminal: true, DeletePod: true,
			Reason:  tunnelv1alpha1.ReasonAccountTunnelLimitReached,
			Message: "jprq account tunnel limit reached; delete a tunnel you no longer need, then delete and re-create this one",
		}

	case jprq.ExitSubdomainBusy:
		// The only code whose handling the spec controls (DESIGN.md §9.3).
		if collisionBehavior == tunnelv1alpha1.CollisionRetry {
			return verdict{
				Reason:  tunnelv1alpha1.ReasonSubdomainBusy,
				Message: "subdomain is already taken; retrying until the current holder releases it",
			}
		}
		return verdict{
			Terminal: true, DeletePod: true,
			Reason:  tunnelv1alpha1.ReasonSubdomainBusy,
			Message: "subdomain is already taken; set subdomainCollisionBehavior: retry, or choose another subdomain",
		}

	case jprq.ExitInvalidSubdomain:
		// Should be unreachable: the schema's pattern and length bounds are the
		// server's own rules (DESIGN.md §5.2). Treated as terminal rather than
		// retried, since retrying an invalid name cannot help.
		return verdict{
			Terminal: true, DeletePod: true,
			Reason:  tunnelv1alpha1.ReasonInvalidSubdomain,
			Message: "jprq rejected the subdomain as invalid, which the CRD schema should have prevented; please report this",
		}

	case jprq.ExitCNAMEBusy:
		// Unreachable in v1: spec.cname is rejected at admission and the agent
		// is never given JPRQ_CNAME.
		return verdict{
			Terminal: true, DeletePod: true,
			Reason:  tunnelv1alpha1.ReasonCNAMEBusy,
			Message: "jprq reported the CNAME is busy, which should be unreachable in v1alpha1; please report this",
		}

	default:
		return verdict{
			Reason:  tunnelv1alpha1.ReasonAgentExited,
			Message: exitDescription(exitCode),
		}
	}
}

// exitDescription explains a retryable exit in terms a human can act on.
func exitDescription(exitCode int32) string {
	switch exitCode {
	case jprq.ExitOK:
		return "agent exited cleanly and is being restarted"
	case jprq.ExitConfigFailure:
		return "agent could not load its configuration or token; retrying"
	case jprq.ExitEventServerUnreachable:
		return "agent could not reach the jprq event server; retrying"
	case jprq.ExitEventStreamDropped:
		// The expected steady-state failure: upstream has no reconnect, so the
		// agent exits and kubelet restarts it (DESIGN.md §6.2).
		return "connection to the jprq event server dropped; reconnecting"
	default:
		return fmt.Sprintf("agent exited with code %d; retrying", exitCode)
	}
}

// agentStatus returns the agent container's status, or nil if kubelet has not
// reported it yet.
func agentStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == agentContainerName {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// classifyPod derives a verdict from a pod's container status, reporting ok=false
// when there is nothing to classify.
//
// The guard that matters: with restartPolicy: Always, LastTerminationState
// persists after a *successful* restart, so a container that is currently
// Running and Ready must never be judged by a termination from several restarts
// ago — that would mark a working tunnel Failed.
func classifyPod(pod *corev1.Pod, collisionBehavior string) (verdict, int32, bool) {
	cs := agentStatus(pod)
	if cs == nil {
		return verdict{}, 0, false
	}
	if cs.State.Running != nil && cs.Ready {
		return verdict{}, 0, false
	}

	term := cs.LastTerminationState.Terminated
	if term == nil {
		term = cs.State.Terminated
	}
	if term == nil {
		// Still pulling, starting, or running but not yet ready.
		return verdict{}, 0, false
	}

	return classifyExit(term.ExitCode, collisionBehavior), term.ExitCode, true
}
