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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tunnelv1alpha1 "github.com/muzaffarnurillaew/jprq-bek/api/v1alpha1"
	"github.com/muzaffarnurillaew/jprq-bek/cli/jprq"
	"github.com/muzaffarnurillaew/jprq-bek/internal/jprqconfig"
)

const (
	testOperatorNamespace = "jprq-system"
	testBackendNamespace  = "default"
	testAgentImage        = "example.test/jprq-agent:v0"
	// Passed as an override so no spec ever reaches the network.
	testDomain = "jprq.test"
)

// specCounter keeps object names unique. JprqTunnel is cluster-scoped and
// envtest runs no garbage collector, so leftovers from one spec would otherwise
// be visible to the next.
var specCounter int

var _ = Describe("JprqTunnel Controller", func() {
	var (
		reconciler  *JprqTunnelReconciler
		tunnelName  string
		secretName  string
		serviceName string
	)

	// ---- helpers ----

	ensureNamespace := func(name string) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}

	createSecret := func(key, value string) {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testOperatorNamespace},
			Data:       map[string][]byte{key: []byte(value)},
		})).To(Succeed())
	}

	createService := func() {
		Expect(k8sClient.Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: testBackendNamespace},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{
					Name:       "http",
					Port:       8080,
					TargetPort: intstr.FromInt32(8080),
				}},
			},
		})).To(Succeed())
	}

	// A nil ready exercises the discovery/v1 contract that nil means ready.
	createEndpointSlice := func(ready *bool) {
		Expect(k8sClient.Create(ctx, &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName + "-slice",
				Namespace: testBackendNamespace,
				Labels:    map[string]string{discoveryv1.LabelServiceName: serviceName},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{"10.244.0.5"},
				Conditions: discoveryv1.EndpointConditions{Ready: ready},
			}},
		})).To(Succeed())
	}

	createTunnel := func(collisionBehavior string) {
		Expect(k8sClient.Create(ctx, &tunnelv1alpha1.JprqTunnel{
			ObjectMeta: metav1.ObjectMeta{Name: tunnelName},
			Spec: tunnelv1alpha1.JprqTunnelSpec{
				Subdomain:                  tunnelName,
				SubdomainCollisionBehavior: collisionBehavior,
				Backend: tunnelv1alpha1.BackendRef{
					Namespace:   testBackendNamespace,
					ServiceName: serviceName,
					Port:        intstr.FromString("http"),
				},
				JprqTokenSecret: tunnelv1alpha1.SecretKeyRef{Name: secretName, Key: defaultTokenKey},
			},
		})).To(Succeed())
	}

	// happyPath builds every precondition the reconciler needs to create a pod.
	happyPath := func() {
		createSecret(defaultTokenKey, "a-real-token")
		createService()
		createEndpointSlice(new(true))
		createTunnel(tunnelv1alpha1.CollisionFail)
	}

	reconcileNow := func() ctrl.Result {
		result, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: tunnelName},
		})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	fetchTunnel := func() *tunnelv1alpha1.JprqTunnel {
		tunnel := &tunnelv1alpha1.JprqTunnel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tunnelName}, tunnel)).To(Succeed())
		return tunnel
	}

	fetchPod := func() (*corev1.Pod, error) {
		pod := &corev1.Pod{}
		err := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: testOperatorNamespace,
			Name:      podName(tunnelName),
		}, pod)
		return pod, err
	}

	expectNoPod := func() {
		_, err := fetchPod()
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected no agent pod to exist")
	}

	podEnv := func(pod *corev1.Pod) map[string]string {
		env := map[string]string{}
		for _, e := range pod.Spec.Containers[0].Env {
			env[e.Name] = e.Value
		}
		return env
	}

	// envtest runs no kubelet, so container status is written by hand. That is
	// also the only way to exercise exit-code classification.
	setAgentStatus := func(status corev1.ContainerStatus) {
		pod, err := fetchPod()
		Expect(err).NotTo(HaveOccurred())
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{status}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	markRunningAndReady := func() {
		setAgentStatus(corev1.ContainerStatus{
			Name:  agentContainerName,
			Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		})
	}

	markCrashLooping := func(exitCode int32) {
		setAgentStatus(corev1.ContainerStatus{
			Name:         agentContainerName,
			Ready:        false,
			RestartCount: 2,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode},
			},
		})
	}

	// forceDeletePod bypasses the grace period. With no kubelet to confirm the
	// deletion, an ordinary delete leaves the pod Terminating forever.
	forceDeletePod := func() {
		pod, err := fetchPod()
		if apierrors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
	}

	condition := func(tunnel *tunnelv1alpha1.JprqTunnel, conditionType string) *metav1.Condition {
		found := apimeta.FindStatusCondition(tunnel.Status.Conditions, conditionType)
		Expect(found).NotTo(BeNil(), "condition %s was never set", conditionType)
		return found
	}

	// ---- lifecycle ----

	BeforeEach(func() {
		specCounter++
		tunnelName = fmt.Sprintf("web-%d", specCounter)
		secretName = fmt.Sprintf("jprq-token-%d", specCounter)
		serviceName = fmt.Sprintf("backend-%d", specCounter)

		ensureNamespace(testOperatorNamespace)

		reconciler = &JprqTunnelReconciler{
			Client:            k8sClient,
			Scheme:            k8sClient.Scheme(),
			Recorder:          events.NewFakeRecorder(128),
			AgentImage:        testAgentImage,
			OperatorNamespace: testOperatorNamespace,
			Domain:            jprqconfig.NewResolver("", testDomain),
		}
	})

	AfterEach(func() {
		// Owner references clean nothing up here: envtest has no garbage
		// collector, so every object is removed explicitly.
		forceDeletePod()

		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &tunnelv1alpha1.JprqTunnel{
			ObjectMeta: metav1.ObjectMeta{Name: tunnelName}}))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testOperatorNamespace}}))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: testBackendNamespace}}))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name: serviceName + "-slice", Namespace: testBackendNamespace}}))).To(Succeed())
	})

	// ---- token preconditions ----

	Context("when the token Secret is unusable", func() {
		It("reports SecretNotFound and creates no pod", func() {
			createService()
			createEndpointSlice(new(true))
			createTunnel(tunnelv1alpha1.CollisionFail)

			reconcileNow()

			tunnel := fetchTunnel()
			Expect(tunnel.Status.Phase).To(Equal(tunnelv1alpha1.PhasePending))
			tokenReady := condition(tunnel, tunnelv1alpha1.ConditionTokenReady)
			Expect(tokenReady.Status).To(Equal(metav1.ConditionFalse))
			Expect(tokenReady.Reason).To(Equal(tunnelv1alpha1.ReasonSecretNotFound))

			// An agent that cannot work must never start: it would still
			// register with jprq and burn one of the account's tunnel slots.
			expectNoPod()
		})

		It("reports SecretKeyNotFound when the named key is absent", func() {
			createSecret("wrongKey", "a-real-token")
			createService()
			createEndpointSlice(new(true))
			createTunnel(tunnelv1alpha1.CollisionFail)

			reconcileNow()

			Expect(condition(fetchTunnel(), tunnelv1alpha1.ConditionTokenReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonSecretKeyNotFound))
			expectNoPod()
		})

		It("reports EmptyToken when the key holds only whitespace", func() {
			createSecret(defaultTokenKey, "   \n")
			createService()
			createEndpointSlice(new(true))
			createTunnel(tunnelv1alpha1.CollisionFail)

			reconcileNow()

			Expect(condition(fetchTunnel(), tunnelv1alpha1.ConditionTokenReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonEmptyToken))
			expectNoPod()
		})
	})

	// ---- backend preconditions ----

	Context("when the backend is not ready", func() {
		It("reports ServiceNotFound and creates no pod", func() {
			createSecret(defaultTokenKey, "a-real-token")
			createTunnel(tunnelv1alpha1.CollisionFail)

			reconcileNow()

			tunnel := fetchTunnel()
			Expect(tunnel.Status.Phase).To(Equal(tunnelv1alpha1.PhasePending))
			Expect(condition(tunnel, tunnelv1alpha1.ConditionBackendReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonServiceNotFound))
			expectNoPod()
		})

		It("reports PortNotResolvable when the Service has no such port name", func() {
			createSecret(defaultTokenKey, "a-real-token")
			Expect(k8sClient.Create(ctx, &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: testBackendNamespace},
				Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
					Name: "grpc", Port: 9090, TargetPort: intstr.FromInt32(9090),
				}}},
			})).To(Succeed())
			createTunnel(tunnelv1alpha1.CollisionFail)

			reconcileNow()

			Expect(condition(fetchTunnel(), tunnelv1alpha1.ConditionBackendReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonPortNotResolvable))
			expectNoPod()
		})

		It("reports NoReadyEndpoints when nothing is behind the Service", func() {
			createSecret(defaultTokenKey, "a-real-token")
			createService()
			createTunnel(tunnelv1alpha1.CollisionFail)

			reconcileNow()

			Expect(condition(fetchTunnel(), tunnelv1alpha1.ConditionBackendReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonNoReadyEndpoints))
			// Without this check Phase would reach Active while every request
			// through the tunnel 502s.
			expectNoPod()
		})

		It("reports NoReadyEndpoints when the only endpoint is explicitly unready", func() {
			createSecret(defaultTokenKey, "a-real-token")
			createService()
			createEndpointSlice(new(false))
			createTunnel(tunnelv1alpha1.CollisionFail)

			reconcileNow()

			Expect(condition(fetchTunnel(), tunnelv1alpha1.ConditionBackendReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonNoReadyEndpoints))
			expectNoPod()
		})

		It("treats a nil Ready condition as ready, per the discovery/v1 contract", func() {
			createSecret(defaultTokenKey, "a-real-token")
			createService()
			createEndpointSlice(nil)
			createTunnel(tunnelv1alpha1.CollisionFail)

			reconcileNow()

			Expect(condition(fetchTunnel(), tunnelv1alpha1.ConditionBackendReady).Status).
				To(Equal(metav1.ConditionTrue))
			_, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())
		})

		It("keeps a running pod when the backend loses its endpoints", func() {
			happyPath()
			reconcileNow()
			markRunningAndReady()
			reconcileNow()
			Expect(fetchTunnel().Status.Phase).To(Equal(tunnelv1alpha1.PhaseActive))

			Expect(k8sClient.Delete(ctx, &discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name: serviceName + "-slice", Namespace: testBackendNamespace}})).To(Succeed())

			reconcileNow()

			tunnel := fetchTunnel()
			Expect(tunnel.Status.Phase).To(Equal(tunnelv1alpha1.PhaseDegraded))
			Expect(condition(tunnel, tunnelv1alpha1.ConditionBackendReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonNoReadyEndpoints))

			// Deliberately asymmetric: deleting the pod would release the
			// subdomain claim, and jprq offers no way to reserve it.
			pod, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.DeletionTimestamp).To(BeNil())
		})
	})

	// ---- pod convergence ----

	Context("when every precondition holds", func() {
		It("creates an agent pod with the resolved backend", func() {
			happyPath()

			reconcileNow()

			pod, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())

			env := podEnv(pod)
			Expect(env["JPRQ_PROTOCOL"]).To(Equal("http"))
			Expect(env["JPRQ_SUBDOMAIN"]).To(Equal(tunnelName))
			Expect(env["JPRQ_BACKEND_HOST"]).
				To(Equal(fmt.Sprintf("%s.%s.svc", serviceName, testBackendNamespace)))
			// The port name "http" resolved to the Service's number, which is
			// why the agent needs no API access of its own.
			Expect(env["JPRQ_BACKEND_PORT"]).To(Equal("8080"))
			Expect(env["JPRQ_TOKEN_FILE"]).To(Equal("/etc/jprq/authToken"))

			Expect(pod.Annotations).To(HaveKey(SpecHashAnnotation))
			Expect(pod.Spec.Containers[0].Image).To(Equal(testAgentImage))

			// A namespaced pod declaring a cluster-scoped owner is what makes
			// garbage collection work despite the CR having no namespace.
			Expect(pod.OwnerReferences).To(HaveLen(1))
			Expect(pod.OwnerReferences[0].Kind).To(Equal("JprqTunnel"))
			Expect(pod.OwnerReferences[0].Name).To(Equal(tunnelName))

			tunnel := fetchTunnel()
			Expect(tunnel.Status.Phase).To(Equal(tunnelv1alpha1.PhasePending))
			Expect(tunnel.Status.PodName).To(Equal(podName(tunnelName)))
			Expect(tunnel.Status.AssignedSubdomain).To(Equal(tunnelName))
			Expect(tunnel.Status.BackendAddress).
				To(Equal(fmt.Sprintf("%s.%s.svc:8080", serviceName, testBackendNamespace)))
			Expect(tunnel.Status.ObservedGeneration).To(Equal(tunnel.Generation))
		})

		It("goes Active with a URL once the agent is ready", func() {
			happyPath()
			reconcileNow()
			markRunningAndReady()

			reconcileNow()

			tunnel := fetchTunnel()
			Expect(tunnel.Status.Phase).To(Equal(tunnelv1alpha1.PhaseActive))
			Expect(tunnel.Status.URL).To(Equal(fmt.Sprintf("https://%s.%s", tunnelName, testDomain)))
			ready := condition(tunnel, tunnelv1alpha1.ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			Expect(ready.Reason).To(Equal(tunnelv1alpha1.ReasonTunnelActive))
		})

		It("is idempotent: a second reconcile leaves the pod alone", func() {
			happyPath()
			reconcileNow()
			first, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())

			reconcileNow()

			second, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())
			Expect(second.UID).To(Equal(first.UID))
			Expect(second.DeletionTimestamp).To(BeNil())
		})

		It("recreates the pod when the spec changes", func() {
			happyPath()
			reconcileNow()
			before, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())

			tunnel := fetchTunnel()
			tunnel.Spec.Subdomain = tunnelName + "-moved"
			Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())

			// The first pass deletes the outdated pod and never patches it: a
			// Pod's env is immutable.
			reconcileNow()
			expectNoPod()

			// The second pass creates its replacement.
			reconcileNow()
			after, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())
			Expect(after.UID).NotTo(Equal(before.UID))
			Expect(after.Annotations[SpecHashAnnotation]).
				NotTo(Equal(before.Annotations[SpecHashAnnotation]))
			Expect(podEnv(after)["JPRQ_SUBDOMAIN"]).To(Equal(tunnelName + "-moved"))
		})

		It("recreates the pod when the token is rotated in place", func() {
			happyPath()
			reconcileNow()
			before, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: secretName, Namespace: testOperatorNamespace}, secret)).To(Succeed())
			secret.Data[defaultTokenKey] = []byte("a-rotated-token")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			// Only the token digest in the spec hash makes this work. Hashing
			// the Secret's name and key alone would ignore a rotation, and the
			// agent reads its token once at startup — so it would keep using
			// the old one indefinitely.
			reconcileNow()
			expectNoPod()

			reconcileNow()
			after, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())
			Expect(after.Annotations[SpecHashAnnotation]).
				NotTo(Equal(before.Annotations[SpecHashAnnotation]))
		})

		It("waits rather than recreating while the old pod is still terminating", func() {
			happyPath()
			reconcileNow()

			// A finalizer holds the pod in Terminating, which is what a real
			// cluster shows while kubelet shuts the container down. envtest has
			// no kubelet, so it would otherwise drop an unscheduled pod at once.
			pod, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())
			pod.Finalizers = append(pod.Finalizers, "jprq.io/test-hold")
			Expect(k8sClient.Update(ctx, pod)).To(Succeed())
			DeferCleanup(func() {
				held, err := fetchPod()
				if apierrors.IsNotFound(err) {
					return
				}
				Expect(err).NotTo(HaveOccurred())
				held.Finalizers = nil
				Expect(client.IgnoreNotFound(k8sClient.Update(ctx, held))).To(Succeed())
			})

			tunnel := fetchTunnel()
			tunnel.Spec.Subdomain = tunnelName + "-moved"
			Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())

			reconcileNow()
			terminating, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())
			Expect(terminating.DeletionTimestamp).NotTo(BeNil())

			reconcileNow()

			// Two agents must never hold one subdomain at once, so no
			// replacement appears until the old pod is really gone.
			Expect(fetchTunnel().Status.Phase).To(Equal(tunnelv1alpha1.PhaseDegraded))
			still, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())
			Expect(still.UID).To(Equal(terminating.UID))
		})
	})

	// ---- exit-code classification ----

	Context("when the agent exits", func() {
		It("marks Failed and deletes the pod on an auth failure", func() {
			happyPath()
			reconcileNow()
			markCrashLooping(jprq.ExitAuthFailed)

			reconcileNow()

			tunnel := fetchTunnel()
			Expect(tunnel.Status.Phase).To(Equal(tunnelv1alpha1.PhaseFailed))
			Expect(tunnel.Status.LastExitCode).To(Equal(new(int32(jprq.ExitAuthFailed))))
			Expect(condition(tunnel, tunnelv1alpha1.ConditionReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonAuthFailed))
			// The server rejected the credential, not the request.
			Expect(condition(tunnel, tunnelv1alpha1.ConditionTokenReady).Status).
				To(Equal(metav1.ConditionFalse))

			// Deleted rather than left to crash-loop, so a terminal failure
			// costs one failed pod and nothing ongoing.
			expectNoPod()
		})

		It("marks Failed on the account tunnel limit without blaming the token", func() {
			happyPath()
			reconcileNow()
			markCrashLooping(jprq.ExitAccountTunnelLimit)

			reconcileNow()

			tunnel := fetchTunnel()
			Expect(tunnel.Status.Phase).To(Equal(tunnelv1alpha1.PhaseFailed))
			Expect(condition(tunnel, tunnelv1alpha1.ConditionReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonAccountTunnelLimitReached))
			Expect(condition(tunnel, tunnelv1alpha1.ConditionTokenReady).Status).
				To(Equal(metav1.ConditionTrue))
		})

		It("marks Failed and deletes the pod on a collision when behavior is fail", func() {
			happyPath()
			reconcileNow()
			markCrashLooping(jprq.ExitSubdomainBusy)

			reconcileNow()

			tunnel := fetchTunnel()
			Expect(tunnel.Status.Phase).To(Equal(tunnelv1alpha1.PhaseFailed))
			Expect(condition(tunnel, tunnelv1alpha1.ConditionReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonSubdomainBusy))
			expectNoPod()
		})

		It("stays Degraded and keeps the pod on a collision when behavior is retry", func() {
			createSecret(defaultTokenKey, "a-real-token")
			createService()
			createEndpointSlice(new(true))
			createTunnel(tunnelv1alpha1.CollisionRetry)

			reconcileNow()
			markCrashLooping(jprq.ExitSubdomainBusy)

			result := reconcileNow()

			tunnel := fetchTunnel()
			Expect(tunnel.Status.Phase).To(Equal(tunnelv1alpha1.PhaseDegraded))
			Expect(condition(tunnel, tunnelv1alpha1.ConditionReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonSubdomainBusy))
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// kubelet's restart backoff is the retry: a container restart is
			// itself a fresh subdomain claim, so deleting the pod here would
			// race its own recovery.
			pod, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.DeletionTimestamp).To(BeNil())
		})

		It("stays Degraded and keeps the pod when the event stream drops", func() {
			happyPath()
			reconcileNow()
			markCrashLooping(jprq.ExitEventStreamDropped)

			reconcileNow()

			tunnel := fetchTunnel()
			Expect(tunnel.Status.Phase).To(Equal(tunnelv1alpha1.PhaseDegraded))
			Expect(condition(tunnel, tunnelv1alpha1.ConditionReady).Reason).
				To(Equal(tunnelv1alpha1.ReasonAgentExited))

			pod, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.DeletionTimestamp).To(BeNil())
		})

		It("ignores a stale termination once the agent is running and ready again", func() {
			happyPath()
			reconcileNow()

			setAgentStatus(corev1.ContainerStatus{
				Name:         agentContainerName,
				Ready:        true,
				RestartCount: 4,
				State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: jprq.ExitAuthFailed},
				},
			})

			reconcileNow()

			// Judging a healthy container by an old termination would mark a
			// working tunnel Failed.
			tunnel := fetchTunnel()
			Expect(tunnel.Status.Phase).To(Equal(tunnelv1alpha1.PhaseActive))
			Expect(tunnel.Status.RestartCount).To(Equal(int32(4)))
		})
	})

	// ---- sticky terminal state ----

	Context("once a tunnel has failed terminally", func() {
		It("does not re-arm without a generation change", func() {
			happyPath()
			reconcileNow()
			markCrashLooping(jprq.ExitAuthFailed)
			reconcileNow()
			Expect(fetchTunnel().Status.Phase).To(Equal(tunnelv1alpha1.PhaseFailed))

			forceDeletePod()
			reconcileNow()

			// Re-applying unchanged YAML must not retry: this is what stops a
			// rejected token from crash-looping forever.
			Expect(fetchTunnel().Status.Phase).To(Equal(tunnelv1alpha1.PhaseFailed))
			expectNoPod()
		})

		It("re-arms after a spec edit", func() {
			happyPath()
			reconcileNow()
			markCrashLooping(jprq.ExitAuthFailed)
			reconcileNow()
			forceDeletePod()

			tunnel := fetchTunnel()
			tunnel.Spec.Subdomain = tunnelName + "-fixed"
			Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())

			reconcileNow()

			Expect(fetchTunnel().Status.Phase).To(Equal(tunnelv1alpha1.PhasePending))
			_, err := fetchPod()
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ---- deletion ----

	It("does nothing once the tunnel is gone", func() {
		happyPath()
		reconcileNow()

		Expect(k8sClient.Delete(ctx, fetchTunnel())).To(Succeed())

		// No finalizer in v1: garbage collection removes the pod through the
		// owner reference, so a reconcile for a missing object is a no-op.
		result, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: tunnelName},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.IsZero()).To(BeTrue())
	})
})

var _ = Describe("JprqTunnel admission validation", func() {
	var counter int

	newTunnel := func(mutate func(*tunnelv1alpha1.JprqTunnel)) *tunnelv1alpha1.JprqTunnel {
		counter++
		tunnel := &tunnelv1alpha1.JprqTunnel{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("admission-%d", counter)},
			Spec: tunnelv1alpha1.JprqTunnelSpec{
				Subdomain: "valid-subdomain",
				Backend: tunnelv1alpha1.BackendRef{
					Namespace:   "default",
					ServiceName: testServiceNameWeb,
					Port:        intstr.FromInt32(80),
				},
				JprqTokenSecret: tunnelv1alpha1.SecretKeyRef{Name: testTokenSecretName},
			},
		}
		mutate(tunnel)
		return tunnel
	}

	// Every subdomain the API accepts must be one the jprq server accepts, so
	// "invalid subdomain" is structurally impossible at runtime rather than a
	// crash-loop to diagnose.
	DescribeTable("rejects specs the jprq server would refuse",
		func(mutate func(*tunnelv1alpha1.JprqTunnel)) {
			Expect(k8sClient.Create(ctx, newTunnel(mutate))).NotTo(Succeed())
		},
		Entry("the reserved subdomain www", func(t *tunnelv1alpha1.JprqTunnel) {
			t.Spec.Subdomain = "www"
		}),
		Entry("the reserved subdomain jprq", func(t *tunnelv1alpha1.JprqTunnel) {
			t.Spec.Subdomain = "jprq"
		}),
		Entry("a subdomain below the minimum length", func(t *tunnelv1alpha1.JprqTunnel) {
			t.Spec.Subdomain = "ab"
		}),
		Entry("a subdomain above the maximum length", func(t *tunnelv1alpha1.JprqTunnel) {
			t.Spec.Subdomain = "a123456789012345678901234567890123456789"
		}),
		Entry("an uppercase subdomain", func(t *tunnelv1alpha1.JprqTunnel) {
			t.Spec.Subdomain = "Web"
		}),
		Entry("a subdomain with consecutive dashes", func(t *tunnelv1alpha1.JprqTunnel) {
			t.Spec.Subdomain = "we--b"
		}),
		Entry("a subdomain with a trailing dash", func(t *tunnelv1alpha1.JprqTunnel) {
			t.Spec.Subdomain = "web-"
		}),
		Entry("the unimplemented addSuffix behavior", func(t *tunnelv1alpha1.JprqTunnel) {
			t.Spec.SubdomainCollisionBehavior = tunnelv1alpha1.CollisionAddSuffix
		}),
		Entry("the unimplemented cname block", func(t *tunnelv1alpha1.JprqTunnel) {
			t.Spec.CNAME = &tunnelv1alpha1.CNAMESpec{
				Hostname:              "tunnel.example.com",
				CloudflareTokenSecret: tunnelv1alpha1.SecretKeyRef{Name: "cf-token"},
			}
		}),
		Entry("a missing backend service name", func(t *tunnelv1alpha1.JprqTunnel) {
			t.Spec.Backend.ServiceName = ""
		}),
		Entry("a missing token Secret name", func(t *tunnelv1alpha1.JprqTunnel) {
			t.Spec.JprqTokenSecret.Name = ""
		}),
	)

	It("accepts a valid spec and defaults the optional fields", func() {
		tunnel := newTunnel(func(*tunnelv1alpha1.JprqTunnel) {})
		Expect(k8sClient.Create(ctx, tunnel)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, tunnel))).To(Succeed())
		})

		Expect(tunnel.Spec.SubdomainCollisionBehavior).To(Equal(tunnelv1alpha1.CollisionFail))
		Expect(tunnel.Spec.JprqTokenSecret.Key).To(Equal(defaultTokenKey))
	})
})
