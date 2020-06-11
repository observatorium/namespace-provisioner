package main

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/go-kit/kit/log"
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

type handler struct {
	c       kubernetes.Interface
	factory informers.SharedInformerFactory
	labels  map[string]string
	logger  log.Logger
	master  *url.URL
	prefix  string
	role    *rbacv1.Role
	token   string
	ttl     time.Duration

	duration *prometheus.HistogramVec
}

func newHander(l log.Logger, r prometheus.Registerer, c kubernetes.Interface, factory informers.SharedInformerFactory, ls map[string]string, master *url.URL, prefix string, role *rbacv1.Role, token string, ttl time.Duration) http.Handler {
	if l == nil {
		l = log.NewNopLogger()
	}

	h := &handler{
		c:       c,
		factory: factory,
		labels:  ls,
		logger:  l,
		master:  master,
		role:    role,
		prefix:  prefix,
		token:   token,
		ttl:     ttl,

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

	return router
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

	sa := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "namespace-provisioner",
			Namespace: namespace,
			Labels:    h.labels,
		},
	}
	sa, err := h.c.CoreV1().ServiceAccounts(namespace).Create(r.Context(), sa, metav1.CreateOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.role.ObjectMeta.Labels = h.labels

	if _, err := h.c.RbacV1().Roles(namespace).Create(r.Context(), h.role, metav1.CreateOptions{}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "namespace-provisioner",
			Namespace: namespace,
			Labels:    h.labels,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     h.role.Kind,
			Name:     h.role.GetName(),
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

	// TODO: Use an informer or something proper as this might race - famous last words
	sa, err = h.c.CoreV1().ServiceAccounts(namespace).Get(r.Context(), sa.Name, metav1.GetOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(sa.Secrets) == 0 {
		msg := "no secret for service account"
		h.logger.Log("msg", msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}

	se, err := h.c.CoreV1().Secrets(namespace).Get(r.Context(), sa.Secrets[0].Name, metav1.GetOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	caCert := se.Data["ca.crt"]
	token := se.Data["token"]

	config := api.NewConfig()
	config.APIVersion = apiv1.SchemeGroupVersion.Version
	config.Kind = "Config"

	cluster := api.NewCluster()
	cluster.Server = "https://56fa0b9d-1d55-4b2d-8d0b-3cb8e4350da9.api.k8s.fr-par.scw.cloud:6443" // TODO
	cluster.CertificateAuthorityData = []byte(caCert)
	config.Clusters["namespace-provisioner"] = cluster

	user := api.NewAuthInfo()
	user.Token = string(token)
	config.AuthInfos["namespace-provisioner"] = user

	context := api.NewContext()
	context.AuthInfo = "namespace-provisioner"
	context.Cluster = "namespace-provisioner"
	context.Namespace = namespace
	config.Contexts["namespace-provisioner"] = context
	config.CurrentContext = "namespace-provisioner"

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
	h.logger.Log("name", name)

	if _, err := h.factory.Core().V1().Namespaces().Lister().Get(name); err != nil {
		if errors.IsNotFound(err) {
			// If the namespace doesn't exit, then it was probably already deleted; respond OK.
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.logger.Log("msg", "deleting namespace", "namespace", name)

	dpf := metav1.DeletePropagationForeground
	if err := h.c.CoreV1().Namespaces().Delete(r.Context(), name, metav1.DeleteOptions{PropagationPolicy: &dpf}); err != nil {
		if errors.IsNotFound(err) {
			// If the namespace doesn't exit, then it was probably already deleted; respond OK.
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}
