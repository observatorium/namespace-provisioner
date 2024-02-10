package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/metalmatze/signal/internalserver"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	flag "github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	logLevelAll   = "all"
	logLevelDebug = "debug"
	logLevelInfo  = "info"
	logLevelWarn  = "warn"
	logLevelError = "error"
	logLevelNone  = "none"

	np = "namespace-provisioner"
)

var (
	availableLogLevels = strings.Join([]string{
		logLevelAll,
		logLevelDebug,
		logLevelInfo,
		logLevelWarn,
		logLevelError,
		logLevelNone,
	}, ", ")
)

// Main is the principal function for the binary, wrapped only by `main` for convenience.
func Main() error {
	kubeconfig := flag.String("kubeconfig", "", "Path to kubeconfig.")
	listen := flag.String("listen", ":8080", "The address at which to to serve the API.")
	listenInternal := flag.String("listen-internal", ":9090", "The address at which to to serve the internal API.")
	logLevel := flag.String("log-level", logLevelInfo, fmt.Sprintf("Log level to use. Possible values: %s", availableLogLevels))
	apiServer := flag.String("server", "https://kubernetes", "The address of the Kubernetes API server to use in generated kubeconfigs.")
	clusterRole := flag.String("cluster-role", "", "The of a Kubernetes ClusterRole to bind to ServiceAccounts in created Namespaces.")
	prefix := flag.String("prefix", "np", "The prefix to use for Namespace names.")
	selector := flag.String("selector", "controller.observatorium.io=namespace-selector", "The label selector to use to select resources.")
	token := flag.String("token", "", "The token to require for authentication with the API.")
	ttl := flag.Duration("ttl", time.Hour, "The default time that a Namespace should exist.")
	flag.Parse()

	l := log.WithPrefix(log.NewJSONLogger(log.NewSyncWriter(os.Stderr)), "name", np)
	l = log.WithPrefix(l, "ts", log.DefaultTimestampUTC)
	l = log.WithPrefix(l, "caller", log.DefaultCaller)

	switch *logLevel {
	case logLevelAll:
		l = level.NewFilter(l, level.AllowAll())
	case logLevelDebug:
		l = level.NewFilter(l, level.AllowDebug())
	case logLevelInfo:
		l = level.NewFilter(l, level.AllowInfo())
	case logLevelWarn:
		l = level.NewFilter(l, level.AllowWarn())
	case logLevelError:
		l = level.NewFilter(l, level.AllowError())
	case logLevelNone:
		l = level.NewFilter(l, level.AllowNone())
	default:
		return fmt.Errorf("log level %v unknown; possible values are: %s", *logLevel, availableLogLevels)
	}

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes config: %v", err)
	}
	c := kubernetes.NewForConfigOrDie(config)

	r := prometheus.NewRegistry()
	r.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	ls, err := labels.ConvertSelectorToLabelsMap(*selector)
	if err != nil {
		return fmt.Errorf("failed to parse label selector: %w", err)
	}
	factory := informers.NewSharedInformerFactoryWithOptions(c, 5*time.Minute, informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
		opts.LabelSelector = *selector
	}))
	// Start the informer factory.
	stop := make(chan struct{})
	level.Info(l).Log("msg", "starting informers")
	factory.Core().V1().Namespaces().Informer()
	factory.Start(stop)
	syncs := factory.WaitForCacheSync(stop)
	if !syncs[reflect.TypeOf(&v1.Namespace{})] {
		return errors.New("failed to sync informer caches")
	}
	level.Info(l).Log("msg", "successfully synced informer caches")

	var g run.Group
	{
		h := internalserver.NewHandler(
			internalserver.WithName(np),
			internalserver.WithPrometheusRegistry(r),
			internalserver.WithPProf(),
		)
		s := http.Server{Addr: *listenInternal, Handler: h}

		g.Add(func() error {
			return s.ListenAndServe()
		}, func(err error) {
			s.Shutdown(context.Background())
		})
	}

	{
		apiServerURL, err := url.Parse(*apiServer)
		if err != nil {
			return err
		}
		h := newHander(l, r, c, factory, ls, apiServerURL, *prefix, *clusterRole, *token, *ttl)
		s := http.Server{Addr: *listen, Handler: h}

		// Start the API server.
		g.Add(func() error {
			level.Info(l).Log("msg", "starting the API server", "address", *listen)
			return s.ListenAndServe()
		}, func(error) {
			s.Shutdown(context.Background())
		})
	}

	{
		// Exit gracefully on SIGINT and SIGTERM.
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGINT, syscall.SIGTERM)
		cancel := make(chan struct{})
		g.Add(func() error {
			for {
				select {
				case <-term:
					level.Info(l).Log("msg", "caught interrupt; gracefully cleaning up; see you next time!")
					return nil
				case <-cancel:
					return nil
				}
			}
		}, func(error) {
			close(cancel)
		})
	}

	{
		// Run the informer factory.
		g.Add(func() error {
			<-stop
			return nil
		}, func(error) {
			close(stop)
		})
	}

	return g.Run()
}

func main() {
	if err := Main(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
