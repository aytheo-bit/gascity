package main

import (
	"bytes"
	"reflect"
	"testing"
)

const triggerClaimCandidates = `[{"id":"work-other","status":"open","metadata":{"gc.routed_to":"worker"}},{"id":"work-trigger","status":"open","metadata":{"gc.routed_to":"worker"}}]`

func TestHookClaimHonorsSessionTrigger(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger string
		want    []string
	}{
		{name: "bound trigger after another ready issue", trigger: "work-trigger", want: []string{"work-trigger"}},
		{name: "bound trigger absent", trigger: "work-absent"},
		{name: "unbound session keeps ordinary first-ready claim", want: []string{"work-other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &turnBoundClaimRecorder{}
			var stdout, stderr bytes.Buffer
			env := []string{}
			if tc.trigger != "" {
				env = []string{"GC_SPAWN_ORIGIN=demand", "GC_TRIGGER_BEAD_ID=" + tc.trigger}
			}
			code := doHookClaim("query", "/rig", hookClaimOptions{
				Assignee:     "worker-1",
				RouteTargets: []string{"worker"},
				Env:          env,
				DrainAck:     true,
				JSON:         true,
			}, rec.ops(t, triggerClaimCandidates), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code = %d; stderr=%s", code, stderr.String())
			}
			if !reflect.DeepEqual(rec.claims, tc.want) {
				t.Fatalf("claims = %v, want %v", rec.claims, tc.want)
			}
			result := decodeTurnBoundResult(t, stdout.String())
			if tc.trigger == "work-absent" && result.Action != "drain" {
				t.Fatalf("absent trigger result = %+v, want drain", result)
			}
		})
	}
}

func TestHookClaimRefusesDemandSessionWithoutLaunchTrigger(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		Env:          []string{"GC_SPAWN_ORIGIN=demand"},
		JSON:         true,
	}, rec.ops(t, triggerClaimCandidates), &stdout, &stderr)
	if code == 0 || len(rec.claims) != 0 {
		t.Fatalf("code=%d claims=%v, want refusal before claim; stderr=%s", code, rec.claims, stderr.String())
	}
}

func TestFederatedHookClaimKeepsLaunchTriggerAcrossStoreEnvs(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	launchEnv := []string{"GC_SPAWN_ORIGIN=demand", "GC_TRIGGER_BEAD_ID=work-trigger"}
	stores := []hookStore{
		{dir: "/city", env: []string{"GC_SPAWN_ORIGIN=demand", "GC_TRIGGER_BEAD_ID=work-other"}},
		{dir: "/rig", env: []string{"GC_SPAWN_ORIGIN=demand", "GC_TRIGGER_BEAD_ID=work-other"}},
	}
	run := func(_ string, dir string, _ []string) (string, error) {
		if dir == "/city" {
			return `[{"id":"work-other","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
		}
		return `[{"id":"work-trigger","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("query", "/city", launchEnv, stores, hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		Env:          launchEnv,
		JSON:         true,
	}, rec.ops(t, ""), run, func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d; stderr=%s", code, stderr.String())
	}
	if !reflect.DeepEqual(rec.claims, []string{"work-trigger"}) {
		t.Fatalf("claims = %v, want only launch trigger", rec.claims)
	}
}

func TestFederatedHookClaimRefusesMissingLaunchTriggerDespiteStoreEnv(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	launchEnv := []string{"GC_SPAWN_ORIGIN=demand"}
	stores := []hookStore{
		{dir: "/city", env: []string{"GC_SPAWN_ORIGIN=demand", "GC_TRIGGER_BEAD_ID=work-other"}},
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("query", "/city", launchEnv, stores, hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		Env:          launchEnv,
		JSON:         true,
	}, rec.ops(t, triggerClaimCandidates), func(string, string, []string) (string, error) {
		return triggerClaimCandidates, nil
	}, func(string, error) {}, &stdout, &stderr)
	if code == 0 || len(rec.claims) != 0 {
		t.Fatalf("code=%d claims=%v, want refusal without store-trigger claim; stderr=%s", code, rec.claims, stderr.String())
	}
}
