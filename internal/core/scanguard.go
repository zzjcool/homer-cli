package core

// ScanProblem is the adapter-independent shape of one scan warning.
type ScanProblem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ScanOutcomeLike is the minimal scan result needed by the M-A root guard.
type ScanOutcomeLike struct {
	Snapshot AdapterSnapshot
	Errors   []ScanProblem
}

// isRootUnreadable prevents an empty snapshot plus scan errors from being
// interpreted as an intentional deletion of every local file.
func isRootUnreadable(outcome ScanOutcomeLike) bool {
	return len(outcome.Snapshot.Categories) == 0 && len(outcome.Errors) > 0
}

// IsRootUnreadable is the exported form shared by adapter/sync/CLI workers.
func IsRootUnreadable(outcome ScanOutcomeLike) bool { return isRootUnreadable(outcome) }

func scanErrorPrefix(rootUnreadable bool) string {
	if rootUnreadable {
		return "adapter root 不可读"
	}
	return "扫描告警"
}

func ScanErrorPrefix(rootUnreadable bool) string { return scanErrorPrefix(rootUnreadable) }

func scanProblemMessage(adapterID string, problem ScanProblem, rootUnreadable bool) string {
	return scanErrorPrefix(rootUnreadable) + ": " + adapterID + " (" + problem.Path + ": " + problem.Message + ")"
}

func ScanProblemMessage(adapterID string, problem ScanProblem, rootUnreadable bool) string {
	return scanProblemMessage(adapterID, problem, rootUnreadable)
}

func scanWarningMessages(adapterID string, outcome ScanOutcomeLike) []string {
	rootUnreadable := isRootUnreadable(outcome)
	messages := make([]string, 0, len(outcome.Errors))
	for _, problem := range outcome.Errors {
		messages = append(messages, scanProblemMessage(adapterID, problem, rootUnreadable))
	}
	return messages
}

func ScanWarningMessages(adapterID string, outcome ScanOutcomeLike) []string {
	return scanWarningMessages(adapterID, outcome)
}
