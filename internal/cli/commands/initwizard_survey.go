package commands

import "github.com/AlecAivazis/survey/v2"

// identityWizardPort is the non-TTY implementation. It deliberately returns
// the checked subset unchanged and never reads stdin, so the old init path can
// safely share the wizard orchestration seam in tests and embeddings.
type identityWizardPort struct{}

func (identityWizardPort) MultiSelect(_ string, _ []WizardOption, checked []string) ([]string, error) {
	return append([]string(nil), checked...), nil
}

// surveyWizardPort adapts survey.MultiSelect to the frozen WizardPort. The
// survey library already provides space-to-toggle, Enter-to-confirm and text
// filtering. Its select-all shortcut is terminal-version dependent; the
// wizard therefore defaults every new choice to checked (the documented
// equivalent of pressing a select-all key) and keeps the interaction stable.
type surveyWizardPort struct{}

func (surveyWizardPort) MultiSelect(message string, options []WizardOption, checked []string) ([]string, error) {
	if len(options) == 0 {
		return []string{}, nil
	}
	labels := make([]string, len(options))
	defaultLabels := make([]string, 0, len(checked))
	checkedSet := make(map[string]struct{}, len(checked))
	for _, value := range checked {
		checkedSet[value] = struct{}{}
	}
	for index, option := range options {
		label := option.Label
		if label == "" {
			label = option.Value
		}
		labels[index] = label
		if _, ok := checkedSet[option.Value]; ok {
			defaultLabels = append(defaultLabels, label)
		}
	}

	selectedLabels := make([]string, 0, len(defaultLabels))
	prompt := &survey.MultiSelect{
		Message: message,
		Options: labels,
		Default: defaultLabels,
		Help:    "空格勾选；Enter 确认；输入文字过滤；默认全选（全选快捷键由终端实现）",
	}
	if err := survey.AskOne(prompt, &selectedLabels); err != nil {
		return nil, err
	}
	selectedSet := make(map[string]struct{}, len(selectedLabels))
	for _, label := range selectedLabels {
		selectedSet[label] = struct{}{}
	}
	result := make([]string, 0, len(selectedLabels))
	for index, label := range labels {
		if _, ok := selectedSet[label]; ok {
			result = append(result, options[index].Value)
		}
	}
	return result, nil
}

// NewDefaultWizardPort returns a real survey port only for a TTY. A non-TTY
// caller gets an identity port, which is intentionally non-blocking.
func NewDefaultWizardPort(isTTY bool) WizardPort {
	if !isTTY {
		return identityWizardPort{}
	}
	return surveyWizardPort{}
}
