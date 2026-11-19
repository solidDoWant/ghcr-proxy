//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

type harborPodList struct {
	Items []struct {
		Metadata struct {
			Name              string    `json:"name"`
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type               string    `json:"type"`
				Status             string    `json:"status"`
				LastTransitionTime time.Time `json:"lastTransitionTime"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				RestartCount int `json:"restartCount"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type harborEventList struct {
	Items []struct {
		Reason         string `json:"reason"`
		Count          int    `json:"count"`
		InvolvedObject struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"involvedObject"`
	} `json:"items"`
}

// logHarborPodReadiness prints, for each Harbor pod, the time from its creation
// to Ready, its container restarts and its failed probes. It only logs: a
// failure to read the cluster is printed and never fails the setup.
func (e *env) logHarborPodReadiness(ctx context.Context) {
	step := waitStep()

	var pods harborPodList

	var events harborEventList

	for _, query := range []struct {
		resource string
		into     any
	}{{"pods", &pods}, {"events", &events}} {
		out, err := e.kubectl(ctx, "", "-n", harborNamespace, "get", query.resource, "-o", "json")
		if err == nil {
			err = json.Unmarshal([]byte(out), query.into)
		}

		if err != nil {
			fmt.Fprintf(os.Stderr, "%s%s: Harbor pod readiness not logged: %v\n", waitLogPrefix, step, err)

			return
		}
	}

	failedProbes := map[string]int{}

	for _, event := range events.Items {
		if event.InvolvedObject.Kind == "Pod" && event.Reason == "Unhealthy" {
			failedProbes[event.InvolvedObject.Name] += max(event.Count, 1)
		}
	}

	lines := []string{}

	for _, pod := range pods.Items {
		restarts := 0
		for _, status := range pod.Status.ContainerStatuses {
			restarts += status.RestartCount
		}

		ready := "not ready"

		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = fmt.Sprintf("ready after %.0fs", condition.LastTransitionTime.Sub(pod.Metadata.CreationTimestamp).Seconds())
			}
		}

		name := pod.Metadata.Name
		lines = append(lines, fmt.Sprintf("%s%s: Harbor pod %s %s (%d restarts, %d failed probes)",
			waitLogPrefix, step, name, ready, restarts, failedProbes[name]))
	}

	sort.Strings(lines)

	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
}
