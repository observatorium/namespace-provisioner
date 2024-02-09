package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/log/level"
	"github.com/google/uuid"
	"github.com/julienschmidt/httprouter"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	apiv1 "k8s.io/client-go/tools/clientcmd/api/v1"
)

func withRequireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.Header.Get("Authorization"), " ")
		if len(parts) != 2 {
			http.Error(w, "invalid Authorization header", http.StatusUnauthorized)
			return
		}

		if parts[1] != token {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type handler struct {
	c            kubernetes.Interface
	factory      informers.SharedInformerFactory
	labels       map[string]string
	logger       log.Logger
	apiServerURL *url.URL
	prefix       string
	clusterRole  string
	ttl          time.Duration

	duration *prometheus.HistogramVec
}

func newHander(l log.Logger, r prometheus.Registerer, c kubernetes.Interface, factory informers.SharedInformerFactory, ls map[string]string, apiServerURL *url.URL, prefix string, clusterRole string, token string, ttl time.Duration) http.Handler {
	if l == nil {
		l = log.NewNopLogger()
	}

	h := &handler{
		c:            c,
		factory:      factory,
		labels:       ls,
		logger:       l,
		apiServerURL: apiServerURL,
		clusterRole:  clusterRole,
		prefix:       prefix,
		ttl:          ttl,

		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "namespace_provisioner_action_duration_seconds",
			Help: "Duration to run each action",
		}, []string{"action"}),
	}

	if r != nil {
		r.MustRegister(h.duration)
	}

	router := httprouter.New()
	router.HandlerFunc(http.MethodPost, "/api/v1/namespace", h.create)
	router.HandlerFunc(http.MethodDelete, "/api/v1/namespace/:name", h.delete)

	if token == "" {
		return router
	}

	return withRequireToken(token, router)
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func(start time.Time) {
		h.duration.WithLabelValues("create").Observe(time.Since(start).Seconds())
	}(start)

	namespace := fmt.Sprintf("%s-%s", h.prefix, uuid.Must(uuid.NewUUID()).String())
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespace,
			Labels: h.labels,
		},
	}
	if _, err := h.c.CoreV1().Namespaces().Create(r.Context(), ns, metav1.CreateOptions{}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Schedule asynchronous deletion of the namespace.
	go func() {
		<-time.After(h.ttl)
		dpf := metav1.DeletePropagationForeground
		if err := h.c.CoreV1().Namespaces().Delete(r.Context(), namespace, metav1.DeleteOptions{PropagationPolicy: &dpf}); err != nil {
			if errors.IsNotFound(err) {
				return
			}
			level.Error(h.logger).Log("msg", "failed to clean up namespace", "err", err)
		}
	}()

	sa := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      np,
			Namespace: namespace,
			Labels:    h.labels,
		},
	}
	sa, err := h.c.CoreV1().ServiceAccounts(namespace).Create(r.Context(), sa, metav1.CreateOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	se := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      np,
			Namespace: namespace,
			Labels:    h.labels,
			Annotations: map[string]string{
				v1.ServiceAccountNameKey: np,
			},
		},
		Type: v1.SecretTypeServiceAccountToken,
	}
	se, err = h.c.CoreV1().Secrets(namespace).Create(r.Context(), se, metav1.CreateOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      np,
			Namespace: namespace,
			Labels:    h.labels,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     h.clusterRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      sa.GetName(),
			Namespace: namespace,
		}},
	}

	if _, err := h.c.RbacV1().RoleBindings(namespace).Create(r.Context(), rb, metav1.CreateOptions{}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Create Kubeconfig
	se, err = h.c.CoreV1().Secrets(namespace).Get(r.Context(), np, metav1.GetOptions{})
	if err != nil {
		msg := "no secret for service account"
		level.Error(h.logger).Log("msg", msg, "err", err)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}

	caCert := se.Data["ca.crt"]
	token := se.Data["token"]

	config := api.NewConfig()
	config.APIVersion = apiv1.SchemeGroupVersion.Version
	config.Kind = "Config"
	config.Clusters[np] = &api.Cluster{
		Server:                   h.apiServerURL.String(),
		CertificateAuthorityData: caCert,
	}
	config.AuthInfos[np] = &api.AuthInfo{
		Token: string(token),
	}
	config.Contexts[np] = &api.Context{
		AuthInfo:  np,
		Cluster:   np,
		Namespace: namespace,
	}
	config.CurrentContext = np

	payload, err := clientcmd.Write(*config)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	w.Write(payload)
}

func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func(start time.Time) {
		h.duration.WithLabelValues("delete").Observe(time.Since(start).Seconds())
	}(start)

	name := httprouter.ParamsFromContext(r.Context()).ByName("name")
	if name == "" {
		http.Error(w, "a namespace name must be specified", http.StatusBadRequest)
		return
	}
	level.Debug(h.logger).Log("name", name)

	if _, err := h.factory.Core().V1().Namespaces().Lister().Get(name); err != nil {
		if errors.IsNotFound(err) {
			// If the namespace doesn't exit, then it was probably already deleted; respond OK.
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	level.Info(h.logger).Log("msg", "deleting namespace", "namespace", name)

	dpf := metav1.DeletePropagationForeground
	if err := h.c.CoreV1().Namespaces().Delete(r.Context(), name, metav1.DeleteOptions{PropagationPolicy: &dpf}); err != nil {
		if errors.IsNotFound(err) {
			// If the namespace doesn't exist, then it was probably already deleted; respond OK.
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}
