package secrets_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/registrygate/internal/secrets"
)

func TestSecretValuesNeverPrint(t *testing.T) {
	const value = "s3cr3t-value"

	secret := secrets.Secret{
		State:           secrets.Present,
		Data:            secrets.Data{"password": []byte(value)},
		ResourceVersion: "42",
	}

	var logs bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.Info("secret", "secret", secret, "data", secret.Data, "pointer", &secret)

	textLogs := bytes.Buffer{}
	slog.New(slog.NewTextHandler(&textLogs, nil)).Info("secret", "secret", secret, "data", secret.Data)

	formatted := []string{
		logs.String(),
		textLogs.String(),
		fmt.Sprint(secret, secret.Data),
		fmt.Sprintf("%v %+v %#v %s %q %x %X", secret, secret, secret, secret, secret, secret, secret),
		fmt.Sprintf("%v %+v %#v %s %q %x %X", secret.Data, secret.Data, secret.Data, secret.Data, secret.Data, secret.Data, secret.Data),
		fmt.Sprintf("%v %+v %#v", &secret, &secret, []secrets.Secret{secret}),
		fmt.Errorf("reading: %v", secret).Error(),
		errors.New(fmt.Sprint(map[string]secrets.Secret{"a": secret})).Error(),
	}
	for _, text := range formatted {
		assert.NotContains(t, text, value)
		assert.NotContains(t, text, fmt.Sprintf("%x", value))
		assert.NotContains(t, text, fmt.Sprintf("%d", []byte(value)))
		assert.Contains(t, text, "[redacted]")
	}

	assert.Contains(t, fmt.Sprint(secret), "42", "the resource version is not secret and helps debugging")
}
