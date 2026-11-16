package webhook

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/solidDoWant/registrygate/internal/config"
)

// NewServer returns the webhook server for cfg with the validator registered. Its certificate watcher reloads the
// key pair when the files change. Start it with its Start method; it needs no leader election.
func NewServer(cfg config.Webhook, scheme *runtime.Scheme, adminNamespaces []string, snapshot SnapshotFunc) (ctrlwebhook.Server, error) {
	host, portText, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("webhook listen address: %w", err)
	}

	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("webhook listen port: %w", err)
	}

	dir := filepath.Dir(cfg.CertFile)
	if filepath.Dir(cfg.KeyFile) != dir {
		return nil, errors.New("webhook certFile and keyFile must be in the same directory")
	}

	server := ctrlwebhook.NewServer(ctrlwebhook.Options{
		Host:     host,
		Port:     port,
		CertDir:  dir,
		CertName: filepath.Base(cfg.CertFile),
		KeyName:  filepath.Base(cfg.KeyFile),
	})

	Register(server, scheme, adminNamespaces, snapshot)

	return server, nil
}
