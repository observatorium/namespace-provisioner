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

	name := fmt.Sprintf("%s-%s", h.prefix, uuid.Must(uuid.NewUUID()).String())
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: h.labels,
		},
	}
	if _, err := h.c.CoreV1().Namespaces().Create(r.Context(), ns, metav1.CreateOptions{}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
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
