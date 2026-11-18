package glob

import "testing"

func TestCIReportsAFailingTest(t *testing.T) {
	t.Fatal("deliberate failure: CI must report this test")
}
