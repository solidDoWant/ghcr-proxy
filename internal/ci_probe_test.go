package internal_test

import "testing"

func TestCIReportsAFailingTestProbe(t *testing.T) { t.Fatal("deliberate failure") }
