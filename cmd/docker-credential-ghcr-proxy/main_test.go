package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunPrintsUsageAndFails(t *testing.T) {
	var stderr bytes.Buffer

	code := run(&stderr)

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "Usage: docker-credential-ghcr-proxy get|store|erase|list")
}
