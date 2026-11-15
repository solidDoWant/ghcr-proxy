package main

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/solidDoWant/ghcr-proxy/internal/config"
	"github.com/solidDoWant/ghcr-proxy/internal/controller"
)

// statusRetryDelay is the pause before the status controller joins leader election again after its manager stops.
const statusRetryDelay = 2 * time.Second

// statusController runs the status controller's manager, and starts a new one whenever it stops while the proxy
// is still running (for example when the status lease is lost during an API server outage). Serving does not
// depend on it.
type statusController struct {
	restConfig *rest.Config
	scheme     *runtime.Scheme
	namespace  string
	grants     config.Grants
	logger     *slog.Logger
}

// run blocks until ctx ends. It returns only after the manager has stopped, which includes releasing the lease.
func (s *statusController) run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := s.runOnce(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("status controller stopped; joining leader election again", "error", err.Error())
		}

		select {
		case <-ctx.Done():
		case <-time.After(statusRetryDelay):
		}
	}
}

func (s *statusController) runOnce(ctx context.Context) error {
	options := controller.ManagerOptions(s.scheme, s.namespace)
	// Controller names are unique per process; each restart registers the controller again.
	options.Controller.SkipNameValidation = new(true)

	manager, err := ctrl.NewManager(s.restConfig, options)
	if err != nil {
		return err
	}

	reconciler := &controller.GHCRAccessPolicyReconciler{Client: manager.GetClient(), Grants: s.grants}
	if err := reconciler.SetupWithManager(manager); err != nil {
		return err
	}

	return manager.Start(ctx)
}
