package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/registrygate/internal/credhelper"
)

func TestTokenFileDefaultsAndOverrides(t *testing.T) {
	t.Setenv(credhelper.TokenFileEnv, "")
	assert.Equal(t, credhelper.DefaultTokenFile, tokenFile())

	t.Setenv(credhelper.TokenFileEnv, "/tmp/other")
	assert.Equal(t, "/tmp/other", tokenFile())
}
