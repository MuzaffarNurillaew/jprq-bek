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
	"bytes"
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	tunnelv1alpha1 "github.com/muzaffarnurillaew/jprq-bek/api/v1alpha1"
	"github.com/muzaffarnurillaew/jprq-bek/internal/jprqconfig"
)

const (
	// Field indexes backing the Secret and Service watches. Without them, every
	// Secret or Service event would mean listing and scanning all tunnels.
	indexBackendService = "spec.backend.serviceRef"
	indexTokenSecret    = "spec.jprqTokenSecret.name"

	// defaultTokenKey mirrors the CRD default, for objects that predate it or
	// bypass defaulting.
	defaultTokenKey = "authToken"

	// Requeue cadences. Watches are the primary trigger; these are a net for
	// missed events, so they are deliberately slow (DESIGN.md §9.1).
	requeueWaiting = time.Minute
	requeueActive  = 5 * time.Minute
)

// JprqTunnelReconciler reconciles a JprqTunnel object
type JprqTunnelReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder uses the events.k8s.io/v1 API, whose Eventf takes a related
	// object and an action alongside the note.
	Recorder recorder.EventRecorder

	// AgentImage is set by the manager, never by the CR: a user-settable image
	// would be a privilege-escalation path given v1 has no tenancy boundary
	// (DESIGN.md §12.2).
	AgentImage string

	// OperatorNamespace holds both the agent pods and the token Secrets. The
	// Pod and Secret caches are restricted to it, so every Get for those kinds
	// must be keyed with it.
	OperatorNamespace string

	// Domain resolves the jprq base domain for status.url.
	Domain *jprqconfig.Resolver
}

// gate is the outcome of one precondition, carrying the reason it failed so the
// Ready condition can mirror the specific cause rather than a generic one.
type gate struct {
	ok      bool
	reason  string
	message string
}

// backendInfo is the resolved backend: a hostname and a port number, never a
// port name (DESIGN.md §9.2).
type backendInfo struct {
	host    string
	port    int32
	address string
}

// +kubebuilder:rbac:groups=tunnel.jprq.io,resources=jprqtunnels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tunnel.jprq.io,resources=jprqtunnels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tunnel.jprq.io,resources=jprqtunnels/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives one JprqTunnel towards having a healthy agent pod, and
// records why it does not when it cannot.
//
// Ordering is deliberate (DESIGN.md §9.2): the terminal-failure check sits
// before any Secret or Service read, so a tunnel with a rejected token or an
// exhausted account costs one Get and nothing more.
func (r *JprqTunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	tunnel := &tunnelv1alpha1.JprqTunnel{}
	if err := r.Get(ctx, req.NamespacedName, tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Garbage collection removes the agent pod through its owner reference, so
	// v1 needs no finalizer.
	if !tunnel.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Terminal states are sticky: cleared only by a spec edit, which bumps
	// generation. Re-applying unchanged YAML does not re-arm a failed tunnel,
	// and that is what stops a rejected token from crash-looping forever
	// (DESIGN.md §5.5, §10.1).
	if tunnel.Status.Phase == tunnelv1alpha1.PhaseFailed &&
		tunnel.Status.ObservedGeneration == tunnel.Generation {
		return ctrl.Result{}, nil
	}

	original := tunnel.DeepCopy()
	result, err := r.reconcile(ctx, tunnel)

	// Status is written on every path, error paths included, so a failure is
	// visible in kubectl rather than only in the manager's log.
	if statusErr := r.patchStatus(ctx, tunnel, original); statusErr != nil {
		if err == nil {
			return ctrl.Result{}, statusErr
		}
		log.Error(statusErr, "could not update status")
	}
	return result, err
}

func (r *JprqTunnelReconciler) reconcile(ctx context.Context, tunnel *tunnelv1alpha1.JprqTunnel) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// v1 always claims exactly what was asked for; addSuffix will write a
	// generated name here instead (DESIGN.md §5.5).
	tunnel.Status.AssignedSubdomain = tunnel.Spec.Subdomain

	// Read the pod once up front: whether it exists decides between Pending and
	// Degraded on every not-ready path below.
	existing, err := r.agentPod(ctx, tunnel)
	if err != nil {
		return ctrl.Result{}, err
	}
	tunnel.Status.PodName = ""
	if existing != nil {
		tunnel.Status.PodName = existing.Name
		if cs := agentStatus(existing); cs != nil {
			tunnel.Status.RestartCount = cs.RestartCount
		}
	}

	token, tokenGate, err := r.checkToken(ctx, tunnel)
	if err != nil {
		return ctrl.Result{}, err
	}
	backend, backendGate, err := r.checkBackend(ctx, tunnel)
	if err != nil {
		return ctrl.Result{}, err
	}

	if backendGate.ok {
		tunnel.Status.BackendAddress = backend.address
	}

	// A pod we already asked to go away. Recreating now would mean two agents
	// briefly holding one subdomain, which jprq forbids, so wait it out.
	if existing != nil && !existing.DeletionTimestamp.IsZero() {
		r.setNotReady(tunnel, tunnelv1alpha1.PhaseDegraded, tunnelv1alpha1.ReasonPodPending,
			"waiting for the previous agent pod to terminate")
		return ctrl.Result{RequeueAfter: requeueWaiting}, nil
	}

	// Converge the pod before anything can return early, so a crash-looping
	// tunnel can still be fixed by editing its spec.
	if tokenGate.ok && backendGate.ok {
		desired, err := desiredPod(tunnel, r.agentConfig(tunnel, backend, token), r.Scheme)
		if err != nil {
			return ctrl.Result{}, err
		}

		if existing == nil {
			if err := r.Create(ctx, desired); err != nil {
				return ctrl.Result{}, fmt.Errorf("creating agent pod: %w", err)
			}
			r.Recorder.Eventf(tunnel, nil, corev1.EventTypeNormal, "PodCreated", "CreatePod",
				"Created agent pod %s/%s", desired.Namespace, desired.Name)
			tunnel.Status.PodName = desired.Name
			r.setNotReady(tunnel, tunnelv1alpha1.PhasePending, tunnelv1alpha1.ReasonPodPending,
				"agent pod created, waiting for it to become ready")
			return ctrl.Result{RequeueAfter: requeueWaiting}, nil
		}

		if existing.Annotations[SpecHashAnnotation] != desired.Annotations[SpecHashAnnotation] {
			// Deleted, never patched: a Pod's env is immutable, and two agents
			// must not hold one subdomain at once.
			if err := r.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("deleting outdated agent pod: %w", err)
			}
			r.Recorder.Eventf(tunnel, nil, corev1.EventTypeNormal, "PodRecreated", "RecreatePod",
				"Agent pod %s/%s is outdated; recreating", existing.Namespace, existing.Name)
			r.setNotReady(tunnel, tunnelv1alpha1.PhasePending, tunnelv1alpha1.ReasonPodPending,
				"configuration changed, recreating the agent pod")
			return ctrl.Result{RequeueAfter: requeueWaiting}, nil
		}
	}

	// A verdict from the jprq server outranks a missing token or unready
	// backend: it is the more specific and more actionable explanation.
	if existing != nil {
		if v, exitCode, ok := classifyPod(existing, tunnel.Spec.SubdomainCollisionBehavior); ok {
			return r.applyVerdict(ctx, tunnel, existing, v, exitCode)
		}
	}

	if !tokenGate.ok || !backendGate.ok {
		// Deliberately asymmetric: no pod is created while a precondition
		// fails, but an already-running pod is never deleted for one. Deleting
		// would release the subdomain claim, and jprq offers no way to reserve
		// it, so we might not win it back (DESIGN.md §9.3).
		failed := tokenGate
		if failed.ok {
			failed = backendGate
		}
		phase := tunnelv1alpha1.PhasePending
		if existing != nil {
			phase = tunnelv1alpha1.PhaseDegraded
		}
		r.setNotReady(tunnel, phase, failed.reason, failed.message)
		return ctrl.Result{RequeueAfter: requeueWaiting}, nil
	}

	if cs := agentStatus(existing); cs != nil && cs.State.Running != nil && cs.Ready {
		tunnel.Status.Phase = tunnelv1alpha1.PhaseActive
		tunnel.Status.LastFailureMessage = ""

		// Cosmetic: a tunnel works whether or not we can name it, so a failed
		// domain lookup must never block anything.
		if domain, err := r.Domain.Domain(ctx); err != nil {
			log.Error(err, "could not resolve the jprq base domain; status.url stays empty")
		} else {
			tunnel.Status.URL = jprqconfig.URLFor(tunnel.Status.AssignedSubdomain, domain)
		}

		apimeta.SetStatusCondition(&tunnel.Status.Conditions, metav1.Condition{
			Type:    tunnelv1alpha1.ConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  tunnelv1alpha1.ReasonTunnelActive,
			Message: "tunnel is online",
		})
		return ctrl.Result{RequeueAfter: requeueActive}, nil
	}

	r.setNotReady(tunnel, tunnelv1alpha1.PhaseDegraded, tunnelv1alpha1.ReasonPodNotReady,
		"agent pod is not ready")
	return ctrl.Result{RequeueAfter: requeueWaiting}, nil
}

// applyVerdict records what the agent's exit means.
func (r *JprqTunnelReconciler) applyVerdict(
	ctx context.Context,
	tunnel *tunnelv1alpha1.JprqTunnel,
	pod *corev1.Pod,
	v verdict,
	exitCode int32,
) (ctrl.Result, error) {
	tunnel.Status.LastExitCode = new(exitCode)
	tunnel.Status.LastFailureMessage = v.Message

	if v.TokenInvalid {
		apimeta.SetStatusCondition(&tunnel.Status.Conditions, metav1.Condition{
			Type:    tunnelv1alpha1.ConditionTokenReady,
			Status:  metav1.ConditionFalse,
			Reason:  v.Reason,
			Message: v.Message,
		})
	}

	if !v.Terminal {
		// kubelet's restart backoff is the only retry engine; the controller
		// must not also delete the pod or the two would race (DESIGN.md §6.2).
		if v.Reason == tunnelv1alpha1.ReasonSubdomainBusy {
			r.Recorder.Eventf(tunnel, nil, corev1.EventTypeWarning, v.Reason,
				"ClaimSubdomain", "%s", v.Message)
		}
		r.setNotReady(tunnel, tunnelv1alpha1.PhaseDegraded, v.Reason, v.Message)
		return ctrl.Result{RequeueAfter: requeueWaiting}, nil
	}

	r.setNotReady(tunnel, tunnelv1alpha1.PhaseFailed, v.Reason, v.Message)
	r.Recorder.Eventf(tunnel, nil, corev1.EventTypeWarning, v.Reason, "StartTunnel", "%s", v.Message)

	if v.DeletePod && pod != nil {
		// Deleted rather than left to crash-loop, so a terminal failure costs
		// one failed pod and nothing ongoing (DESIGN.md §10).
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting failed agent pod: %w", err)
		}
		tunnel.Status.PodName = ""
	}

	// No requeue: only a generation change re-arms a terminal failure.
	return ctrl.Result{}, nil
}

// checkToken validates the token Secret and returns the raw token.
func (r *JprqTunnelReconciler) checkToken(
	ctx context.Context,
	tunnel *tunnelv1alpha1.JprqTunnel,
) ([]byte, gate, error) {
	fail := func(reason, message string) gate {
		apimeta.SetStatusCondition(&tunnel.Status.Conditions, metav1.Condition{
			Type:    tunnelv1alpha1.ConditionTokenReady,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		})
		return gate{reason: reason, message: message}
	}

	// The Secret cache is restricted to OperatorNamespace, which is also where
	// spec.jprqTokenSecret is defined to resolve (DESIGN.md §5.2). Keying the
	// Get with any other namespace would not even return a clean NotFound.
	key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: tunnel.Spec.JprqTokenSecret.Name}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, key, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, gate{}, fmt.Errorf("reading Secret %s: %w", key, err)
		}
		return nil, fail(tunnelv1alpha1.ReasonSecretNotFound,
			fmt.Sprintf("Secret %q not found in namespace %q", key.Name, key.Namespace)), nil
	}

	secretKey := tunnel.Spec.JprqTokenSecret.Key
	if secretKey == "" {
		secretKey = defaultTokenKey
	}

	token, present := secret.Data[secretKey]
	if !present {
		return nil, fail(tunnelv1alpha1.ReasonSecretKeyNotFound,
			fmt.Sprintf("Secret %q has no key %q", key.Name, secretKey)), nil
	}
	if len(bytes.TrimSpace(token)) == 0 {
		return nil, fail(tunnelv1alpha1.ReasonEmptyToken,
			fmt.Sprintf("key %q of Secret %q is empty", secretKey, key.Name)), nil
	}

	apimeta.SetStatusCondition(&tunnel.Status.Conditions, metav1.Condition{
		Type:    tunnelv1alpha1.ConditionTokenReady,
		Status:  metav1.ConditionTrue,
		Reason:  tunnelv1alpha1.ReasonTokenFound,
		Message: fmt.Sprintf("using key %q of Secret %q", secretKey, key.Name),
	})
	return token, gate{ok: true}, nil
}

// checkBackend resolves the backend Service to a host and port number and
// requires at least one ready endpoint behind it.
//
// Requiring a ready endpoint is what keeps Phase=Active honest: without it a
// tunnel whose backend has no pods would report Active while every request 502s.
func (r *JprqTunnelReconciler) checkBackend(
	ctx context.Context,
	tunnel *tunnelv1alpha1.JprqTunnel,
) (backendInfo, gate, error) {
	fail := func(reason, message string) gate {
		apimeta.SetStatusCondition(&tunnel.Status.Conditions, metav1.Condition{
			Type:    tunnelv1alpha1.ConditionBackendReady,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		})
		return gate{reason: reason, message: message}
	}

	ref := tunnel.Spec.Backend
	svc := &corev1.Service{}
	key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.ServiceName}
	if err := r.Get(ctx, key, svc); err != nil {
		if !apierrors.IsNotFound(err) {
			return backendInfo{}, gate{}, fmt.Errorf("reading Service %s: %w", key, err)
		}
		return backendInfo{}, fail(tunnelv1alpha1.ReasonServiceNotFound,
			fmt.Sprintf("Service %s not found", key)), nil
	}

	port, err := resolveServicePort(svc, ref.Port)
	if err != nil {
		return backendInfo{}, fail(tunnelv1alpha1.ReasonPortNotResolvable, err.Error()), nil
	}

	ready, err := r.hasReadyEndpoint(ctx, svc)
	if err != nil {
		return backendInfo{}, gate{}, err
	}
	if !ready {
		return backendInfo{}, fail(tunnelv1alpha1.ReasonNoReadyEndpoints,
			fmt.Sprintf("Service %s has no ready endpoints", key)), nil
	}

	// ".svc" rather than ".svc.cluster.local": it resolves through the pod's
	// search list on any cluster domain, not just the default one.
	info := backendInfo{
		host: fmt.Sprintf("%s.%s.svc", svc.Name, svc.Namespace),
		port: port,
	}
	info.address = fmt.Sprintf("%s:%d", info.host, info.port)

	apimeta.SetStatusCondition(&tunnel.Status.Conditions, metav1.Condition{
		Type:    tunnelv1alpha1.ConditionBackendReady,
		Status:  metav1.ConditionTrue,
		Reason:  tunnelv1alpha1.ReasonEndpointsReady,
		Message: fmt.Sprintf("forwarding to %s", info.address),
	})
	return info, gate{ok: true}, nil
}

// resolveServicePort turns a Service port name or number into a number.
//
// A number is validated against the Service rather than passed through, because
// a port the Service does not declare cannot carry traffic to its ClusterIP —
// better to say so here than to let the agent connect and 502.
func resolveServicePort(svc *corev1.Service, want intstr.IntOrString) (int32, error) {
	if want.Type == intstr.Int {
		wanted := int32(want.IntValue())
		for _, p := range svc.Spec.Ports {
			if p.Port == wanted {
				return p.Port, nil
			}
		}
		return 0, fmt.Errorf("port %d is not declared by Service %s/%s", wanted, svc.Namespace, svc.Name)
	}

	for _, p := range svc.Spec.Ports {
		if p.Name == want.StrVal {
			return p.Port, nil
		}
	}
	return 0, fmt.Errorf("no port named %q is declared by Service %s/%s", want.StrVal, svc.Namespace, svc.Name)
}

// hasReadyEndpoint reports whether any EndpointSlice for the Service has a ready
// endpoint.
func (r *JprqTunnelReconciler) hasReadyEndpoint(ctx context.Context, svc *corev1.Service) (bool, error) {
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices,
		client.InNamespace(svc.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name},
	); err != nil {
		return false, fmt.Errorf("listing EndpointSlices for Service %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	for i := range slices.Items {
		for _, endpoint := range slices.Items[i].Endpoints {
			// A nil Ready means ready, per the discovery/v1 contract. Testing
			// `!= nil && *Ready` instead would silently count every nil-Ready
			// endpoint as unready.
			if endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready {
				return true, nil
			}
		}
	}
	return false, nil
}

// agentPod returns the tunnel's agent pod, or nil when it does not exist.
func (r *JprqTunnelReconciler) agentPod(
	ctx context.Context,
	tunnel *tunnelv1alpha1.JprqTunnel,
) (*corev1.Pod, error) {
	// Keyed with OperatorNamespace because the Pod cache is restricted to it.
	key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: podName(tunnel.Name)}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, key, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading agent pod %s: %w", key, err)
	}
	return pod, nil
}

func (r *JprqTunnelReconciler) agentConfig(
	tunnel *tunnelv1alpha1.JprqTunnel,
	backend backendInfo,
	token []byte,
) agentPodConfig {
	secretKey := tunnel.Spec.JprqTokenSecret.Key
	if secretKey == "" {
		secretKey = defaultTokenKey
	}
	return agentPodConfig{
		Image:             r.AgentImage,
		OperatorNamespace: r.OperatorNamespace,
		BackendHost:       backend.host,
		BackendPort:       backend.port,
		Subdomain:         tunnel.Status.AssignedSubdomain,
		SecretName:        tunnel.Spec.JprqTokenSecret.Name,
		SecretKey:         secretKey,
		TokenDigest:       tokenDigest(token),
	}
}

func (r *JprqTunnelReconciler) setNotReady(tunnel *tunnelv1alpha1.JprqTunnel, phase, reason, message string) {
	tunnel.Status.Phase = phase
	apimeta.SetStatusCondition(&tunnel.Status.Conditions, metav1.Condition{
		Type:    tunnelv1alpha1.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
}

// patchStatus writes status only when something changed, using a merge patch
// computed against the pre-mutation copy.
func (r *JprqTunnelReconciler) patchStatus(ctx context.Context, tunnel, original *tunnelv1alpha1.JprqTunnel) error {
	tunnel.Status.ObservedGeneration = tunnel.Generation
	if apiequality.Semantic.DeepEqual(original.Status, tunnel.Status) {
		return nil
	}
	return r.Status().Patch(ctx, tunnel, client.MergeFrom(original))
}

func serviceRefKey(namespace, name string) string {
	return namespace + "/" + name
}

// SetupWithManager sets up the controller with the Manager.
func (r *JprqTunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.OperatorNamespace == "" {
		return fmt.Errorf("JprqTunnelReconciler.OperatorNamespace must be set")
	}
	if r.AgentImage == "" {
		return fmt.Errorf("JprqTunnelReconciler.AgentImage must be set")
	}
	if r.Domain == nil {
		return fmt.Errorf("JprqTunnelReconciler.Domain must be set")
	}

	ctx := context.Background()
	indexer := mgr.GetFieldIndexer()

	if err := indexer.IndexField(ctx, &tunnelv1alpha1.JprqTunnel{}, indexBackendService,
		func(obj client.Object) []string {
			tunnel, ok := obj.(*tunnelv1alpha1.JprqTunnel)
			if !ok {
				return nil
			}
			return []string{serviceRefKey(tunnel.Spec.Backend.Namespace, tunnel.Spec.Backend.ServiceName)}
		},
	); err != nil {
		return fmt.Errorf("indexing %s: %w", indexBackendService, err)
	}

	if err := indexer.IndexField(ctx, &tunnelv1alpha1.JprqTunnel{}, indexTokenSecret,
		func(obj client.Object) []string {
			tunnel, ok := obj.(*tunnelv1alpha1.JprqTunnel)
			if !ok {
				return nil
			}
			return []string{tunnel.Spec.JprqTokenSecret.Name}
		},
	); err != nil {
		return fmt.Errorf("indexing %s: %w", indexTokenSecret, err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&tunnelv1alpha1.JprqTunnel{}).
		// Owned pods are what make this continuous rather than polling: a
		// container exiting becomes a reconcile within milliseconds. Cluster-
		// scoped owners are handled natively — the RESTMapper tells the handler
		// to emit a request with no namespace (DESIGN.md §9.1).
		Owns(&corev1.Pod{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.tunnelsForSecret)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.tunnelsForService)).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(r.tunnelsForEndpointSlice)).
		Named("jprqtunnel").
		Complete(r)
}

func (r *JprqTunnelReconciler) tunnelsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.OperatorNamespace {
		return nil
	}
	return r.requestsByIndex(ctx, indexTokenSecret, obj.GetName())
}

func (r *JprqTunnelReconciler) tunnelsForService(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.requestsByIndex(ctx, indexBackendService, serviceRefKey(obj.GetNamespace(), obj.GetName()))
}

func (r *JprqTunnelReconciler) tunnelsForEndpointSlice(ctx context.Context, obj client.Object) []reconcile.Request {
	// EndpointSlices name their Service with a label, not an owner reference.
	serviceName := obj.GetLabels()[discoveryv1.LabelServiceName]
	if serviceName == "" {
		return nil
	}
	return r.requestsByIndex(ctx, indexBackendService, serviceRefKey(obj.GetNamespace(), serviceName))
}

// requestsByIndex looks up tunnels through a field index. Requests carry no
// namespace, because JprqTunnel is cluster-scoped.
func (r *JprqTunnelReconciler) requestsByIndex(ctx context.Context, index, value string) []reconcile.Request {
	var tunnels tunnelv1alpha1.JprqTunnelList
	if err := r.List(ctx, &tunnels, client.MatchingFields{index: value}); err != nil {
		logf.FromContext(ctx).Error(err, "could not map watch event to tunnels",
			"index", index, "value", value)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(tunnels.Items))
	for i := range tunnels.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: tunnels.Items[i].Name},
		})
	}
	return requests
}
