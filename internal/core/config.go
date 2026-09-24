package core

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

var ageRecipientPattern = regexp.MustCompile(`^age1[02-9ac-hj-np-z]{58}$`)
var secretNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var bareAllowEscapePatterns = map[string]struct{}{
	"*":   {},
	"*/":  {},
	"**":  {},
	"**/": {},
}

func decodeJSONValue(data []byte) (orderedjson.Value, error) {
	return orderedjson.Parse(data)
}

func objectMap(value any) (map[string]orderedjson.Value, bool) {
	object, ok := value.(*orderedjson.Object)
	if !ok || object == nil || object.M == nil {
		return nil, false
	}
	return object.M, true
}

func objectKeys(value any) []string {
	object, ok := value.(*orderedjson.Object)
	if !ok || object == nil {
		return nil
	}
	keys := make([]string, 0, len(object.M))
	seen := make(map[string]struct{}, len(object.M))
	for _, key := range object.Keys {
		if _, exists := object.M[key]; exists {
			keys = append(keys, key)
			seen[key] = struct{}{}
		}
	}
	missing := make([]string, 0)
	for key := range object.M {
		if _, exists := seen[key]; !exists {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return append(keys, missing...)
}

func stringSlice(value any) ([]string, bool) {
	items, ok := value.([]orderedjson.Value)
	if !ok {
		return nil, false
	}
	result := make([]string, len(items))
	for index, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		result[index] = text
	}
	return result, true
}

func jsonValue(value any) string {
	if value == nil {
		return "null"
	}
	if ordered, ok := value.(orderedjson.Value); ok {
		return string(orderedjson.Serialize(ordered))
	}
	return fmt.Sprintf("%v", value)
}

func checkStringArray(value any, where string, validationErrors *[]string) {
	items, ok := stringSlice(value)
	if !ok {
		*validationErrors = append(*validationErrors, fmt.Sprintf("%s 必须是字符串数组", where))
		return
	}
	for index, item := range items {
		if item == "" {
			*validationErrors = append(*validationErrors, fmt.Sprintf("%s[%d] 必须是非空字符串", where, index))
		}
	}
}

func checkOptionalStringArray(value any, present bool, where string, validationErrors *[]string) {
	if !present {
		return
	}
	checkStringArray(value, where, validationErrors)
}

func checkOptionalBoolean(value any, present bool, where string, validationErrors *[]string) {
	if !present {
		return
	}
	if _, ok := value.(bool); !ok {
		*validationErrors = append(*validationErrors, fmt.Sprintf("%s 必须是布尔值", where))
	}
}

func normalizeGlob(pattern string) string {
	for strings.HasPrefix(pattern, "./") {
		pattern = strings.TrimPrefix(pattern, "./")
	}
	for strings.HasPrefix(pattern, "/") {
		pattern = strings.TrimPrefix(pattern, "/")
	}
	return pattern
}

func checkAllowEscape(value any, where string, validationErrors *[]string) {
	checkStringArray(value, where, validationErrors)
	items, ok := stringSlice(value)
	if !ok {
		return
	}
	for index, item := range items {
		if _, bare := bareAllowEscapePatterns[normalizeGlob(item)]; bare {
			*validationErrors = append(*validationErrors, fmt.Sprintf(
				"%s[%d] 是裸通配模式（%s）：它会放行 root 下任意路径的逃逸链接，安全边界失效；请改成具体路径（如 \"skills/agent-browser\" 或带尾 \"/\" 的具体目录前缀）",
				where, index, jsonValue(item),
			))
		}
	}
}

func validateCategory(raw any, where string, validationErrors *[]string) {
	object, ok := objectMap(raw)
	if !ok {
		*validationErrors = append(*validationErrors, fmt.Sprintf("%s 必须是对象", where))
		return
	}

	paths, exists := object["paths"]
	if !exists {
		*validationErrors = append(*validationErrors, fmt.Sprintf("%s.paths 必须是非空字符串数组", where))
	} else {
		items, validArray := stringSlice(paths)
		if !validArray || len(items) == 0 {
			*validationErrors = append(*validationErrors, fmt.Sprintf("%s.paths 必须是非空字符串数组", where))
		} else {
			checkStringArray(paths, where+".paths", validationErrors)
		}
	}

	mode, exists := object["mode"]
	modeText, modeIsString := mode.(string)
	if !exists || !modeIsString || (modeText != string(SyncModeMerge) && modeText != string(SyncModeMirror)) {
		*validationErrors = append(*validationErrors, fmt.Sprintf("%s.mode 必须是 'merge' 或 'mirror'（当前: %s）", where, jsonValue(mode)))
	}

	enabled, enabledPresent := object["enabled"]
	checkOptionalBoolean(enabled, enabledPresent, where+".enabled", validationErrors)
	exclude, excludePresent := object["exclude"]
	checkOptionalStringArray(exclude, excludePresent, where+".exclude", validationErrors)
	excludeKeys, excludeKeysPresent := object["excludeKeys"]
	checkOptionalStringArray(excludeKeys, excludeKeysPresent, where+".excludeKeys", validationErrors)
}

func validateAdapter(raw any, where string, validationErrors *[]string) {
	object, ok := objectMap(raw)
	if !ok {
		*validationErrors = append(*validationErrors, fmt.Sprintf("%s 必须是对象", where))
		return
	}

	root, exists := object["root"]
	rootText, rootIsString := root.(string)
	if !exists || !rootIsString || rootText == "" {
		*validationErrors = append(*validationErrors, fmt.Sprintf("%s.root 必须是非空字符串", where))
	}

	categories, exists := object["categories"]
	if !exists {
		*validationErrors = append(*validationErrors, fmt.Sprintf("%s.categories 必须是对象", where))
	} else if categoryMap, validObject := objectMap(categories); !validObject {
		*validationErrors = append(*validationErrors, fmt.Sprintf("%s.categories 必须是对象", where))
	} else {
		for name, category := range categoryMap {
			validateCategory(category, fmt.Sprintf("%s.categories.%s", where, name), validationErrors)
		}
	}

	enabled, enabledPresent := object["enabled"]
	checkOptionalBoolean(enabled, enabledPresent, where+".enabled", validationErrors)
	ignore, ignorePresent := object["ignore"]
	checkOptionalStringArray(ignore, ignorePresent, where+".ignore", validationErrors)
	allowEscape, allowEscapePresent := object["allowEscape"]
	if allowEscapePresent {
		checkAllowEscape(allowEscape, where+".allowEscape", validationErrors)
	}
}

func numberAsFloat(value any) (float64, bool) {
	text, ok := value.(fmt.Stringer)
	if !ok {
		return 0, false
	}
	number, err := strconv.ParseFloat(text.String(), 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}

func isPositiveInteger(value any) bool {
	number, ok := numberAsFloat(value)
	return ok && number > 0 && math.Trunc(number) == number
}

func isNumberOne(value any) bool {
	number, ok := numberAsFloat(value)
	return ok && number == 1
}

func validateBackup(raw any, present bool, validationErrors *[]string) {
	if !present {
		return
	}
	object, ok := objectMap(raw)
	if !ok {
		*validationErrors = append(*validationErrors, "backup 必须是对象")
		return
	}
	keep, exists := object["keep"]
	if exists && !isPositiveInteger(keep) {
		*validationErrors = append(*validationErrors, fmt.Sprintf("backup.keep 必须是正整数（当前: %s）", jsonValue(keep)))
	}
}

func validateSecrets(raw any, present bool, validationErrors *[]string) {
	if !present {
		return
	}
	object, ok := objectMap(raw)
	if !ok {
		*validationErrors = append(*validationErrors, "secrets 必须是对象")
		return
	}

	ignorePaths, ignorePathsPresent := object["ignorePaths"]
	checkOptionalStringArray(ignorePaths, ignorePathsPresent, "secrets.ignorePaths", validationErrors)

	if recipients, exists := object["recipients"]; exists {
		checkStringArray(recipients, "secrets.recipients", validationErrors)
		if items, validArray := stringSlice(recipients); validArray {
			for index, item := range items {
				if item != "" && !ageRecipientPattern.MatchString(item) {
					*validationErrors = append(*validationErrors, fmt.Sprintf(
						"secrets.recipients[%d] 不是合法的 age recipient（应为 age1 + 58 字符）", index,
					))
				}
			}
		}
	}

	files, exists := object["files"]
	if !exists {
		return
	}
	fileMap, validObject := objectMap(files)
	if !validObject {
		*validationErrors = append(*validationErrors, "secrets.files 必须是对象（secret 名 -> 目标路径）")
		return
	}
	for name, destination := range fileMap {
		if !secretNamePattern.MatchString(name) {
			*validationErrors = append(*validationErrors, fmt.Sprintf(
				"secrets.files 的键 %q 不是合法的 secret 名（应形如 [A-Za-z0-9][A-Za-z0-9._-]*）", name,
			))
		}
		text, isString := destination.(string)
		if !isString || text == "" {
			*validationErrors = append(*validationErrors, fmt.Sprintf("secrets.files.%s 必须是非空字符串", name))
		} else if !strings.HasPrefix(text, "~") && !strings.HasPrefix(text, "/") {
			*validationErrors = append(*validationErrors, fmt.Sprintf(
				"secrets.files.%s 必须以 '~' 或 '/' 开头（当前: %s）", name, jsonValue(text),
			))
		}
	}
}

func validateConfigObject(raw orderedjson.Value) (*HomerConfig, []string) {
	object, ok := objectMap(raw)
	if !ok {
		return nil, []string{"homer.json 顶层必须是对象"}
	}

	validationErrors := make([]string, 0)
	version, exists := object["version"]
	if !exists || !isNumberOne(version) {
		validationErrors = append(validationErrors, fmt.Sprintf("version 必须是 1（当前: %s）", jsonValue(version)))
	}

	adapters, exists := object["adapters"]
	if !exists {
		validationErrors = append(validationErrors, "adapters 必须是对象")
	} else if adapterMap, validObject := objectMap(adapters); !validObject {
		validationErrors = append(validationErrors, "adapters 必须是对象")
	} else {
		for adapterID, adapter := range adapterMap {
			validateAdapter(adapter, "adapters."+adapterID, &validationErrors)
		}
	}

	backup, backupPresent := object["backup"]
	validateBackup(backup, backupPresent, &validationErrors)
	secrets, secretsPresent := object["secrets"]
	validateSecrets(secrets, secretsPresent, &validationErrors)
	if len(validationErrors) > 0 {
		return nil, validationErrors
	}

	config, err := configFromValue(object)
	if err != nil {
		return nil, []string{fmt.Sprintf("homer.json 配置无法读取: %v", err)}
	}
	return config, nil
}

func stringValue(object map[string]orderedjson.Value, key string) string {
	value, _ := object[key].(string)
	return value
}

func boolPointer(object map[string]orderedjson.Value, key string) *bool {
	value, ok := object[key].(bool)
	if !ok {
		return nil
	}
	return &value
}

func stringsFromValue(value orderedjson.Value) []string {
	items, _ := stringSlice(value)
	return items
}

func intFromNumber(value orderedjson.Value) (int, error) {
	text, ok := value.(fmt.Stringer)
	if !ok {
		return 0, errors.New("不是数字")
	}
	raw := text.String()
	if integer, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if integer <= 0 || int64(int(integer)) != integer {
			return 0, errors.New("不是正整数")
		}
		return int(integer), nil
	}
	floating, err := strconv.ParseFloat(raw, 64)
	if err != nil || floating <= 0 || math.Trunc(floating) != floating || floating > float64(int(^uint(0)>>1)) {
		return 0, errors.New("不是正整数")
	}
	return int(floating), nil
}

func configFromValue(root map[string]orderedjson.Value) (*HomerConfig, error) {
	config := &HomerConfig{Version: 1, Adapters: map[string]AdapterConfig{}}
	adaptersObject, ok := objectMap(root["adapters"])
	if !ok {
		return nil, errors.New("adapters 不是对象")
	}
	for adapterID, rawAdapter := range adaptersObject {
		adapterObject, ok := objectMap(rawAdapter)
		if !ok {
			return nil, errors.New("adapter 不是对象")
		}
		adapter := AdapterConfig{
			Root:       stringValue(adapterObject, "root"),
			Enabled:    boolPointer(adapterObject, "enabled"),
			Categories: map[string]CategoryConfig{},
		}
		if ignore, exists := adapterObject["ignore"]; exists {
			adapter.Ignore = stringsFromValue(ignore)
		}
		if allowEscape, exists := adapterObject["allowEscape"]; exists {
			adapter.AllowEscape = stringsFromValue(allowEscape)
		}
		categoriesObject, ok := objectMap(adapterObject["categories"])
		if !ok {
			return nil, errors.New("categories 不是对象")
		}
		for categoryName, rawCategory := range categoriesObject {
			categoryObject, ok := objectMap(rawCategory)
			if !ok {
				return nil, errors.New("category 不是对象")
			}
			category := CategoryConfig{
				Paths:   stringsFromValue(categoryObject["paths"]),
				Mode:    SyncMode(stringValue(categoryObject, "mode")),
				Enabled: boolPointer(categoryObject, "enabled"),
			}
			if exclude, exists := categoryObject["exclude"]; exists {
				category.Exclude = stringsFromValue(exclude)
			}
			if excludeKeys, exists := categoryObject["excludeKeys"]; exists {
				category.ExcludeKeys = stringsFromValue(excludeKeys)
			}
			adapter.Categories[categoryName] = category
		}
		config.Adapters[adapterID] = adapter
	}

	if rawBackup, exists := root["backup"]; exists {
		backupObject, ok := objectMap(rawBackup)
		if !ok {
			return nil, errors.New("backup 不是对象")
		}
		backup := &BackupConfig{}
		if keep, exists := backupObject["keep"]; exists {
			value, err := intFromNumber(keep)
			if err != nil {
				return nil, err
			}
			backup.Keep = &value
		}
		config.Backup = backup
	}
	if rawSecrets, exists := root["secrets"]; exists {
		secretsObject, ok := objectMap(rawSecrets)
		if !ok {
			return nil, errors.New("secrets 不是对象")
		}
		secrets := &SecretsConfig{}
		if ignorePaths, exists := secretsObject["ignorePaths"]; exists {
			secrets.IgnorePaths = stringsFromValue(ignorePaths)
		}
		if recipients, exists := secretsObject["recipients"]; exists {
			secrets.Recipients = stringsFromValue(recipients)
		}
		if files, exists := secretsObject["files"]; exists {
			fileObject, ok := objectMap(files)
			if !ok {
				return nil, errors.New("secrets.files 不是对象")
			}
			secrets.Files = make(map[string]string, len(fileObject))
			for name, destination := range fileObject {
				secrets.Files[name], _ = destination.(string)
			}
		}
		config.Secrets = secrets
	}
	return config, nil
}

// ValidateConfig validates a raw homer.json document and returns all shape
// errors without panicking. It is the only public config validation boundary.
func ValidateConfig(raw []byte) (*HomerConfig, []string) {
	value, err := decodeJSONValue(raw)
	if err != nil {
		return nil, []string{fmt.Sprintf("homer.json 不是合法 JSON: %v", err)}
	}
	return validateConfigObject(value)
}

// ValidateConfigValue is a convenience for tests and internal callers that
// already have an ordered JSON value.
func ValidateConfigValue(raw orderedjson.Value) (*HomerConfig, []string) {
	return validateConfigObject(raw)
}

// LoadConfig reads and validates homer.json. A missing file returns the
// os.ErrNotExist sentinel; malformed or invalid files include their path.
func LoadConfig(paths HomerPaths) (*HomerConfig, error) {
	data, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	config, validationErrors := ValidateConfig(data)
	if config == nil {
		if len(validationErrors) == 1 && strings.HasPrefix(validationErrors[0], "homer.json 不是合法 JSON:") {
			return nil, fmt.Errorf("%s 不是合法 JSON: %s", paths.ConfigFile, strings.TrimPrefix(validationErrors[0], "homer.json 不是合法 JSON: "))
		}
		return nil, fmt.Errorf("%s 配置无效:\n  - %s", paths.ConfigFile, strings.Join(validationErrors, "\n  - "))
	}
	return config, nil
}

func loadConfig(paths HomerPaths) (*HomerConfig, error) { return LoadConfig(paths) }

func orderedNumber(text string) orderedjson.Value {
	value, err := orderedjson.Parse([]byte(text))
	if err != nil {
		return nil
	}
	return value
}

func orderedStringSlice(values []string) orderedjson.Value {
	items := make([]orderedjson.Value, len(values))
	for index, value := range values {
		items[index] = value
	}
	return items
}

func orderedObject(keys []string, values map[string]orderedjson.Value) orderedjson.Value {
	return &orderedjson.Object{Keys: keys, M: values}
}

func configToValue(config HomerConfig) orderedjson.Value {
	adapterValues := make(map[string]orderedjson.Value, len(config.Adapters))
	adapterKeys := make([]string, 0, len(config.Adapters))
	for adapterID := range config.Adapters {
		adapterKeys = append(adapterKeys, adapterID)
	}
	sort.Strings(adapterKeys)
	for _, adapterID := range adapterKeys {
		adapter := config.Adapters[adapterID]
		categoryValues := make(map[string]orderedjson.Value, len(adapter.Categories))
		categoryKeys := make([]string, 0, len(adapter.Categories))
		for categoryName := range adapter.Categories {
			categoryKeys = append(categoryKeys, categoryName)
		}
		sort.Strings(categoryKeys)
		for _, categoryName := range categoryKeys {
			category := adapter.Categories[categoryName]
			keys := []string{"paths", "mode"}
			values := map[string]orderedjson.Value{"paths": orderedStringSlice(category.Paths), "mode": string(category.Mode)}
			if category.Enabled != nil {
				keys = append(keys, "enabled")
				values["enabled"] = *category.Enabled
			}
			if category.Exclude != nil {
				keys = append(keys, "exclude")
				values["exclude"] = orderedStringSlice(category.Exclude)
			}
			if category.ExcludeKeys != nil {
				keys = append(keys, "excludeKeys")
				values["excludeKeys"] = orderedStringSlice(category.ExcludeKeys)
			}
			categoryValues[categoryName] = orderedObject(keys, values)
		}
		keys := []string{"root"}
		values := map[string]orderedjson.Value{"root": adapter.Root}
		if adapter.Enabled != nil {
			keys = append(keys, "enabled")
			values["enabled"] = *adapter.Enabled
		}
		keys = append(keys, "categories")
		values["categories"] = orderedObject(categoryKeys, categoryValues)
		if adapter.Ignore != nil {
			keys = append(keys, "ignore")
			values["ignore"] = orderedStringSlice(adapter.Ignore)
		}
		if adapter.AllowEscape != nil {
			keys = append(keys, "allowEscape")
			values["allowEscape"] = orderedStringSlice(adapter.AllowEscape)
		}
		adapterValues[adapterID] = orderedObject(keys, values)
	}

	keys := []string{"version", "adapters"}
	values := map[string]orderedjson.Value{"version": orderedNumber("1"), "adapters": orderedObject(adapterKeys, adapterValues)}
	if config.Backup != nil {
		backupKeys := make([]string, 0, 1)
		backupValues := make(map[string]orderedjson.Value)
		if config.Backup.Keep != nil {
			backupKeys = append(backupKeys, "keep")
			backupValues["keep"] = orderedNumber(strconv.Itoa(*config.Backup.Keep))
		}
		keys = append(keys, "backup")
		values["backup"] = orderedObject(backupKeys, backupValues)
	}
	if config.Secrets != nil {
		secretKeys := make([]string, 0, 3)
		secretValues := make(map[string]orderedjson.Value)
		if config.Secrets.IgnorePaths != nil {
			secretKeys = append(secretKeys, "ignorePaths")
			secretValues["ignorePaths"] = orderedStringSlice(config.Secrets.IgnorePaths)
		}
		if config.Secrets.Recipients != nil {
			secretKeys = append(secretKeys, "recipients")
			secretValues["recipients"] = orderedStringSlice(config.Secrets.Recipients)
		}
		if config.Secrets.Files != nil {
			fileNames := make([]string, 0, len(config.Secrets.Files))
			for name := range config.Secrets.Files {
				fileNames = append(fileNames, name)
			}
			sort.Strings(fileNames)
			fileValues := make(map[string]orderedjson.Value, len(fileNames))
			for _, name := range fileNames {
				fileValues[name] = config.Secrets.Files[name]
			}
			secretKeys = append(secretKeys, "files")
			secretValues["files"] = orderedObject(fileNames, fileValues)
		}
		keys = append(keys, "secrets")
		values["secrets"] = orderedObject(secretKeys, secretValues)
	}
	return orderedObject(keys, values)
}

// SaveConfig atomically writes a validated homer.json with two-space
// indentation and a trailing newline.
func SaveConfig(paths HomerPaths, config HomerConfig) error {
	value := configToValue(config)
	if validated, validationErrors := ValidateConfig(orderedjson.Serialize(value)); validated == nil {
		return fmt.Errorf("拒绝写入非法配置:\n  - %s", strings.Join(validationErrors, "\n  - "))
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o777); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", paths.ConfigFile, os.Getpid())
	if err := os.WriteFile(tmp, orderedjson.SerializeFile(value), 0o666); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, paths.ConfigFile); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func saveConfig(paths HomerPaths, config HomerConfig) error { return SaveConfig(paths, config) }

// ConfigValue converts a typed config into the ordered JSON representation used
// by the repository-wide JSON boundary.
func ConfigValue(config HomerConfig) orderedjson.Value { return configToValue(config) }
