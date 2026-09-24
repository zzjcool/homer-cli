package commands

import "testing"

func TestFrozenCommandExitCodeTable(t *testing.T) {
	pushCases := []struct {
		status PushStatus
		want   int
	}{
		{PushStatusPushed, 0},
		{PushStatusNoDrift, 0},
		{PushStatusSecretsRejected, 1},
		{PushStatusRemoteAhead, 1},
		{PushStatusConflicts, 1},
		{PushStatusAborted, 1},
		{PushStatusError, 1},
	}
	for _, test := range pushCases {
		t.Run("push/"+string(test.status), func(t *testing.T) {
			if got := newPushCommandReport(test.status).ExitCode(); got != test.want {
				t.Fatalf("push %s exit = %d, want %d (m2-plan §2.8)", test.status, got, test.want)
			}
		})
	}

	pullCases := []struct {
		status PullStatus
		want   int
	}{
		{PullStatusApplied, 0},
		{PullStatusNoDrift, 0},
		{PullStatusAborted, 1},
		{PullStatusConflictsRemain, 1},
		{PullStatusError, 1},
	}
	for _, test := range pullCases {
		t.Run("pull/"+string(test.status), func(t *testing.T) {
			if got := newPullCommandReport(test.status).ExitCode(); got != test.want {
				t.Fatalf("pull %s exit = %d, want %d (m2-plan §2.8)", test.status, got, test.want)
			}
		})
	}

	mergeCases := []struct {
		status MergeStatus
		want   int
	}{
		{MergeStatusResolved, 0},
		{MergeStatusNoConflicts, 0},
		{MergeStatusAborted, 1},
		{MergeStatusError, 1},
	}
	for _, test := range mergeCases {
		t.Run("merge/"+string(test.status), func(t *testing.T) {
			if got := newMergeCommandReport(test.status).ExitCode(); got != test.want {
				t.Fatalf("merge %s exit = %d, want %d (m2-plan §2.8)", test.status, got, test.want)
			}
		})
	}

	homeCases := []struct {
		status HomeStatus
		want   int
	}{
		{HomeStatusHomed, 0},
		{HomeStatusAborted, 1},
		{HomeStatusError, 1},
	}
	for _, test := range homeCases {
		t.Run("home/"+string(test.status), func(t *testing.T) {
			if got := newHomeReport(test.status).ExitCode(); got != test.want {
				t.Fatalf("home %s exit = %d, want %d (m3-plan §2.6)", test.status, got, test.want)
			}
		})
	}

	secretPushCases := []struct {
		status SecretPushStatus
		want   int
	}{
		{SecretPushStatusPushed, 0},
		{SecretPushStatusNoSecrets, 0},
		{SecretPushStatusNoIdentity, 1},
		{SecretPushStatusNoRecipients, 1},
		{SecretPushStatusMissing, 1},
		{SecretPushStatusAborted, 1},
		{SecretPushStatusError, 1},
	}
	for _, test := range secretPushCases {
		t.Run("secret-push/"+string(test.status), func(t *testing.T) {
			if got := newPushReport(test.status).ExitCode(); got != test.want {
				t.Fatalf("secret push %s exit = %d, want %d (m3-plan §2.6)", test.status, got, test.want)
			}
		})
	}

	secretPullCases := []struct {
		status SecretPullStatus
		want   int
	}{
		{SecretPullStatusApplied, 0},
		{SecretPullStatusNoSecrets, 0},
		{SecretPullStatusNoIdentity, 1},
		{SecretPullStatusMissingVault, 1},
		{SecretPullStatusUndecryptable, 1},
		{SecretPullStatusAborted, 1},
		{SecretPullStatusError, 1},
	}
	for _, test := range secretPullCases {
		t.Run("secret-pull/"+string(test.status), func(t *testing.T) {
			if got := newPullReport(test.status).ExitCode(); got != test.want {
				t.Fatalf("secret pull %s exit = %d, want %d (m3-plan §2.6)", test.status, got, test.want)
			}
		})
	}

	keygenCases := []struct {
		name string
		ok   bool
		want int
	}{
		{"created", true, 0},
		{"already-exists-or-error", false, 1},
	}
	for _, test := range keygenCases {
		t.Run("secret-keygen/"+test.name, func(t *testing.T) {
			if got := (SecretKeygenReport{OK: test.ok}).ExitCode(); got != test.want {
				t.Fatalf("secret keygen ok=%v exit = %d, want %d (m3-plan §2.6)", test.ok, got, test.want)
			}
		})
	}
	if got := (SecretListReport{}).ExitCode(); got != 0 {
		t.Fatalf("secret list exit = %d, want 0 (m3-plan §2.6)", got)
	}
}
