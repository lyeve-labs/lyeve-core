package schema

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// ValidationError is a single schema validation failure.
type ValidationError struct {
	Field   string         `json:"field"`
	Code    i18n.Code      `json:"code"`
	Rule    string         `json:"rule"`
	Value   any            `json:"value,omitempty"`
	Message string         `json:"message,omitempty"`
	Params  map[string]any `json:"-"`
}

// ValidationErrors is a collection of ValidationError produced by ValidateContent.
type ValidationErrors []ValidationError

// Respond writes ValidationErrors as a JSON array of localized error objects to w,
// using the Accept-Language resolved locale from the request context.
func (ve ValidationErrors) Respond(w http.ResponseWriter, r *http.Request, status int) {
	loc := i18n.LocaleFromCtx(r.Context())
	type localized struct {
		Field   string `json:"field"`
		Message string `json:"message"`
		Code    string `json:"code"`
		Rule    string `json:"rule"`
	}
	out := make([]localized, len(ve))
	for i, e := range ve {
		code := e.Code
		if code == "" {
			code = i18n.CodeValidationUnknownField
		}
		params := e.Params
		if params == nil {
			params = map[string]any{"field": e.Field}
		} else if _, ok := params["field"]; !ok {
			params["field"] = e.Field
		}
		out[i] = localized{
			Field:   e.Field,
			Message: code.Error(loc, params),
			Code:    string(code),
			Rule:    e.Rule,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": out})
}

// typeMismatch reports the type a field expects when the value it was given
// cannot become that type, and "" when the value is acceptable.
//
// Only the write path's two broken outcomes are refused here, and neither has
// a caller it can break:
//
//   - A JSON boolean or number for a text-shaped column reaches the driver as
//     a Go bool or float64, which has no encode plan for text. The write would
//     fail as 503 Service Unavailable, telling the caller the service was down
//     because it sent the wrong JSON type.
//   - Anything but a JSON boolean for a boolean column is coerced by the
//     database. "yes" becomes true and the row holds true, while the response
//     would echo back "yes", so the API would report storing a value it had
//     not stored.
//
// An object or an array for a text column is left alone: that is stored as its
// JSON text, and callers rely on it.
//
// A field declared with no type at all is left alone too. It generates a text
// column, but the two write paths do not agree about what a field is:
// /api/v1/content/{schema} binds values to that column, while
// /api/admin/content stores the whole body as JSON and never touches it.
// Holding an untyped field to the column's rule would break the documented
// enum contract, where the validator stringifies via fmt.Sprint so a numeric 1
// matches the string "1". A schema that says what its fields are gets the
// check.
func typeMismatch(fieldType string, v any) string {
	switch fieldType {
	case "boolean":
		if _, ok := v.(bool); !ok {
			return "boolean"
		}
	case "text", "rich_text", "email", "url", "media", "uid", "date", "datetime":
		switch v.(type) {
		case bool, float64, float32, int, int64, json.Number:
			return "text"
		}
	case "number":
		if _, ok := v.(bool); ok {
			return "number"
		}
	}
	return ""
}

// ValidateContent runs all field-level, cross-field, and custom validators
// defined on the schema against the given content data map. It returns nil
// when all rules pass.
func ValidateContent(schema *domain.Schema, data map[string]any) []ValidationError {
	return validate(schema, data, data, false)
}

// ValidatePatch validates the body of a partial update, which writes only the
// fields it carries and leaves every other stored value as it is.
//
// A field the patch leaves out is not checked at all, so a required field
// already stored stays satisfied. A field the patch does carry gets every
// rule a create would apply: an explicit null or empty value for a required
// field is refused, since writing it would clear the field, and the type,
// enum, length and custom rules all run.
//
// A cross-field rule compares two fields that need not both be in the patch,
// so it is evaluated against current with the patch laid over it, and only
// when the patch names one of its targets. A rule the stored row already
// broke is not charged to an update that never touched its fields.
func ValidatePatch(schema *domain.Schema, patch, current map[string]any) []ValidationError {
	merged := make(map[string]any, len(current)+len(patch))
	for k, v := range current {
		merged[k] = v
	}
	for k, v := range patch {
		merged[k] = v
	}
	return validate(schema, patch, merged, true)
}

// validate checks data against the schema. With partial set, a field absent
// from data is skipped and a cross-field rule runs only when data names one
// of its targets. Cross-field rules read crossData.
func validate(schema *domain.Schema, data, crossData map[string]any, partial bool) []ValidationError {
	if schema == nil {
		return nil
	}
	var errs []ValidationError

	fieldIdx := indexFieldsByName(schema.Fields)

	for _, field := range schema.Fields {
		if field.System {
			continue
		}
		if field.FieldType == "relation" {
			continue
		}

		val, exists := data[field.Name]
		if partial && !exists {
			continue
		}

		// Required check.
		if field.Required && isEmpty(val) {
			errs = append(errs, ValidationError{
				Field:  field.Name,
				Code:   i18n.CodeValidationRequired,
				Rule:   "required",
				Value:  val,
				Params: map[string]any{"field": field.Name},
			})
			continue
		}

		// Empty optional fields skip remaining validators.
		if !exists || isEmpty(val) {
			continue
		}

		// A value the column cannot hold is caught here, where the caller
		// learns which field is wrong, rather than at the driver.
		if expected := typeMismatch(field.FieldType, val); expected != "" {
			errs = append(errs, ValidationError{
				Field:  field.Name,
				Code:   i18n.CodeValidationFieldType,
				Rule:   "field_type",
				Value:  val,
				Params: map[string]any{"field": field.Name, "expected": expected},
			})
			continue
		}

		// An email field is an address whether or not the schema also names
		// the email rule. PostgreSQL refuses a malformed one at the column.
		// MySQL and SQL Server store whatever arrives, so the check lives here
		// where every dialect passes through it.
		if field.FieldType == "email" && !hasRule(field.Validation, "email") {
			if err := validateEmail(field.Name, val); err != nil {
				errs = append(errs, *err)
			}
		}

		for _, rule := range field.Validation {
			if err := validateFieldValue(field.Name, val, rule); err != nil {
				errs = append(errs, *err)
			}
		}
	}

	for _, rule := range schema.CrossFieldValidation {
		if partial && !namesAnyTarget(data, rule.Targets) {
			continue
		}
		if cerr := validateCrossField(fieldIdx, crossData, rule); cerr != nil {
			errs = append(errs, *cerr)
		}
	}

	customIdx := indexCustomValidators(schema.CustomValidators)
	for _, field := range schema.Fields {
		if field.System || field.FieldType == "relation" {
			continue
		}
		val, exists := data[field.Name]
		if !exists || isEmpty(val) {
			continue
		}
		for _, rule := range field.Validation {
			if !strings.HasPrefix(rule.Rule, "custom:") {
				continue
			}
			name := strings.TrimPrefix(rule.Rule, "custom:")
			cv, ok := customIdx[name]
			if !ok {
				errs = append(errs, ValidationError{
					Field:   field.Name,
					Code:    i18n.CodeSchemaValidationCrossField,
					Rule:    rule.Rule,
					Message: fmt.Sprintf("unknown custom validator %q", name),
				})
				continue
			}
			result := runCustomValidator(cv, val, rule.Params)
			if !result.OK {
				errs = append(errs, ValidationError{
					Field:   field.Name,
					Code:    i18n.CodeSchemaValidationCrossField,
					Rule:    rule.Rule,
					Message: result.Error,
					Value:   val,
				})
			}
		}
	}
	return errs
}

func namesAnyTarget(data map[string]any, targets []string) bool {
	for _, t := range targets {
		if _, ok := data[t]; ok {
			return true
		}
	}
	return false
}

func validateFieldValue(fieldName string, val any, rule domain.ValidationRule) *ValidationError {
	switch rule.Rule {
	case "email":
		return validateEmail(fieldName, val)
	case "url":
		return validateURL(fieldName, val)
	case "regex":
		return validateRegex(fieldName, val, rule)
	case "enum":
		return validateEnum(fieldName, val, rule)
	case "min":
		return validateMin(fieldName, val, rule)
	case "max":
		return validateMax(fieldName, val, rule)
	case "range":
		return validateRange(fieldName, val, rule)
	default:
		if strings.HasPrefix(rule.Rule, "custom:") {
			return nil
		}
		return &ValidationError{
			Field:  fieldName,
			Code:   i18n.CodeSchemaValidationCrossField,
			Rule:   rule.Rule,
			Params: map[string]any{"field": fieldName, "rule": rule.Rule},
		}
	}
}

func validateEmail(fieldName string, val any) *ValidationError {
	s := toString(val)
	if _, err := mail.ParseAddress(s); err != nil {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationEmail, Rule: "email", Value: val, Params: map[string]any{"field": fieldName}}
	}
	if !strings.Contains(s, "@") || !strings.Contains(s, ".") {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationEmail, Rule: "email", Value: val, Params: map[string]any{"field": fieldName}}
	}
	return nil
}

func validateURL(fieldName string, val any) *ValidationError {
	s := toString(val)
	u, err := url.ParseRequestURI(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationURL, Rule: "url", Value: val, Params: map[string]any{"field": fieldName}}
	}
	return nil
}

func validateRegex(fieldName string, val any, rule domain.ValidationRule) *ValidationError {
	s := toString(val)
	pattern, _ := rule.Params["pattern"].(string)
	if pattern == "" {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationRegex, Rule: "regex", Params: map[string]any{"field": fieldName}}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationRegex, Rule: "regex", Value: pattern, Params: map[string]any{"field": fieldName}}
	}
	if !re.MatchString(s) {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationRegex, Rule: "regex", Value: val, Params: map[string]any{"field": fieldName}}
	}
	return nil
}

func validateEnum(fieldName string, val any, rule domain.ValidationRule) *ValidationError {
	allowed := rule.Params["values"]
	if allowed == nil {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationEnum, Rule: "enum", Params: map[string]any{"field": fieldName}}
	}

	var values []any
	switch v := allowed.(type) {
	case []any:
		values = v
	default:
		rv := reflect.ValueOf(allowed)
		if rv.Kind() == reflect.Slice {
			values = make([]any, rv.Len())
			for i := range rv.Len() {
				values[i] = rv.Index(i).Interface()
			}
		}
	}

	if len(values) == 0 {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationEnum, Rule: "enum", Params: map[string]any{"field": fieldName}}
	}

	for _, candidate := range values {
		if fmt.Sprint(val) == fmt.Sprint(candidate) {
			return nil
		}
	}
	valueStrs := make([]string, len(values))
	for i, v := range values {
		valueStrs[i] = fmt.Sprint(v)
	}
	return &ValidationError{
		Field:  fieldName,
		Code:   i18n.CodeSchemaValidationEnum,
		Rule:   "enum",
		Value:  val,
		Params: map[string]any{"field": fieldName, "values": strings.Join(valueStrs, ", ")},
	}
}

func validateMin(fieldName string, val any, rule domain.ValidationRule) *ValidationError {
	minRaw, ok := rule.Params["min"]
	if !ok {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationMin, Rule: "min", Params: map[string]any{"field": fieldName}}
	}
	minVal, err := toFloat(minRaw)
	if err != nil {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationMin, Rule: "min", Params: map[string]any{"field": fieldName, "min": fmt.Sprint(minRaw)}}
	}
	n, isNum, _ := toNumeric(val)
	if isNum {
		if n < minVal {
			return minError(fieldName, n, minVal)
		}
		return nil
	}
	s := toString(val)
	if float64(len(s)) < minVal {
		return minError(fieldName, float64(len(s)), minVal)
	}
	return nil
}

func validateMax(fieldName string, val any, rule domain.ValidationRule) *ValidationError {
	maxRaw, ok := rule.Params["max"]
	if !ok {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationMax, Rule: "max", Params: map[string]any{"field": fieldName}}
	}
	maxVal, err := toFloat(maxRaw)
	if err != nil {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationMax, Rule: "max", Params: map[string]any{"field": fieldName, "max": fmt.Sprint(maxRaw)}}
	}
	n, isNum, _ := toNumeric(val)
	if isNum {
		if n > maxVal {
			return maxError(fieldName, n, maxVal)
		}
		return nil
	}
	s := toString(val)
	if float64(len(s)) > maxVal {
		return maxError(fieldName, float64(len(s)), maxVal)
	}
	return nil
}

func validateRange(fieldName string, val any, rule domain.ValidationRule) *ValidationError {
	minRaw, minOK := rule.Params["min"]
	maxRaw, maxOK := rule.Params["max"]
	if !minOK || !maxOK {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationRange, Rule: "range", Params: map[string]any{"field": fieldName}}
	}
	minVal, err := toFloat(minRaw)
	if err != nil {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationRange, Rule: "range", Params: map[string]any{"field": fieldName, "min": fmt.Sprint(minRaw)}}
	}
	maxVal, err := toFloat(maxRaw)
	if err != nil {
		return &ValidationError{Field: fieldName, Code: i18n.CodeSchemaValidationRange, Rule: "range", Params: map[string]any{"field": fieldName, "max": fmt.Sprint(maxRaw)}}
	}
	n, isNum, _ := toNumeric(val)
	if isNum {
		if n < minVal || n > maxVal {
			return &ValidationError{
				Field:  fieldName,
				Code:   i18n.CodeSchemaValidationRange,
				Rule:   "range",
				Value:  val,
				Params: map[string]any{"field": fieldName, "min": fmt.Sprint(minVal), "max": fmt.Sprint(maxVal), "value": fmt.Sprint(val)},
			}
		}
		return nil
	}
	s := toString(val)
	if float64(len(s)) < minVal || float64(len(s)) > maxVal {
		return &ValidationError{
			Field:  fieldName,
			Code:   i18n.CodeSchemaValidationRange,
			Rule:   "range",
			Value:  val,
			Params: map[string]any{"field": fieldName, "min": fmt.Sprint(minVal), "max": fmt.Sprint(maxVal), "value": fmt.Sprint(len(s))},
		}
	}
	return nil
}

func validateCrossField(fieldIdx map[string]domain.SchemaField, data map[string]any, rule domain.CrossFieldRule) *ValidationError {
	switch rule.Rule {
	case "required_with":
		return validateRequiredWith(data, rule)
	case "lt_field", "gt_field", "lte_field", "gte_field":
		return validateCompareFields(fieldIdx, data, rule)
	default:
		return &ValidationError{
			Field:  strings.Join(rule.Targets, ","),
			Code:   i18n.CodeSchemaValidationCrossField,
			Rule:   rule.Rule,
			Params: map[string]any{"rule": rule.Rule},
		}
	}
}

func validateRequiredWith(data map[string]any, rule domain.CrossFieldRule) *ValidationError {
	if len(rule.Targets) < 2 {
		return &ValidationError{
			Field:  "cross_field",
			Code:   i18n.CodeSchemaValidationRequiredWith,
			Rule:   "required_with",
			Params: map[string]any{"rule": "required_with"},
		}
	}
	primary := rule.Targets[0]
	other := rule.Targets[1]

	otherVal, otherExists := data[other]
	if !otherExists || isEmpty(otherVal) {
		return nil
	}

	primaryVal, primaryExists := data[primary]
	if !primaryExists || isEmpty(primaryVal) {
		return &ValidationError{
			Field:  primary,
			Code:   i18n.CodeSchemaValidationRequiredWith,
			Rule:   "required_with",
			Params: map[string]any{"field": primary, "other": other},
		}
	}
	return nil
}

func validateCompareFields(fieldIdx map[string]domain.SchemaField, data map[string]any, rule domain.CrossFieldRule) *ValidationError {
	if len(rule.Targets) < 2 {
		return &ValidationError{
			Field:  "cross_field",
			Code:   i18n.CodeSchemaValidationCrossField,
			Rule:   rule.Rule,
			Params: map[string]any{"rule": rule.Rule},
		}
	}
	fieldA := rule.Targets[0]
	fieldB := rule.Targets[1]

	aVal, aExists := data[fieldA]
	bVal, bExists := data[fieldB]
	if !aExists || !bExists {
		return nil
	}

	// The field types decide how the values order. The kernel and the schema
	// engine both validate content, so they share one comparison rather than
	// each keeping a copy that drifts.
	cmp, err := core.CompareFieldValues(fieldIdx[fieldA].FieldType, fieldIdx[fieldB].FieldType, aVal, bVal)
	if err != nil {
		return &ValidationError{
			Field:  fmt.Sprintf("%s,%s", fieldA, fieldB),
			Code:   i18n.CodeSchemaValidationCrossField,
			Rule:   rule.Rule,
			Params: map[string]any{"rule": rule.Rule},
		}
	}
	if core.CompareRuleHolds(rule.Rule, cmp) {
		return nil
	}
	switch rule.Rule {
	case core.RuleLtField:
		return compareError(fieldA, fieldB, "less than")
	case core.RuleGtField:
		return compareError(fieldA, fieldB, "greater than")
	case core.RuleLteField:
		return compareError(fieldA, fieldB, "less than or equal to")
	}
	return compareError(fieldA, fieldB, "greater than or equal to")
}

func indexCustomValidators(cvs []domain.CustomValidator) map[string]domain.CustomValidator {
	idx := make(map[string]domain.CustomValidator, len(cvs))
	for _, cv := range cvs {
		idx[cv.Name] = cv
	}
	return idx
}

// runCustomValidator resolves the validator a schema entry names and runs it
// against the value. The registry answers with the built-in pattern
// interpreter when no plugin has claimed the name, so an entry whose body is a
// regular expression works on an install with no plugin at all.
func runCustomValidator(cv domain.CustomValidator, val any, params map[string]any) core.CustomValidatorResult {
	fn, ok := core.LookupCustomValidator(cv.Name)
	if !ok {
		return core.CustomValidatorResult{
			Error: fmt.Sprintf("custom validator %q is not registered", cv.Name),
		}
	}
	return fn(val, cv.Body, params)
}

func indexFieldsByName(fields []domain.SchemaField) map[string]domain.SchemaField {
	m := make(map[string]domain.SchemaField, len(fields))
	for _, f := range fields {
		m[f.Name] = f
	}
	return m
}

func isEmpty(val any) bool {
	if val == nil {
		return true
	}
	switch v := val.(type) {
	case string:
		return v == ""
	case bool:
		return false
	case json.Number:
		return v.String() == ""
	default:
		rv := reflect.ValueOf(val)
		switch rv.Kind() {
		case reflect.Slice, reflect.Map, reflect.Array:
			return rv.Len() == 0
		case reflect.Ptr, reflect.Interface:
			return rv.IsNil()
		}
		return false
	}
}

func toString(val any) string {
	switch v := val.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return fmt.Sprint(val)
	}
}

func toNumeric(val any) (float64, bool, error) {
	switch v := val.(type) {
	case float64:
		return v, true, nil
	case float32:
		return float64(v), true, nil
	case int:
		return float64(v), true, nil
	case int8:
		return float64(v), true, nil
	case int16:
		return float64(v), true, nil
	case int32:
		return float64(v), true, nil
	case int64:
		return float64(v), true, nil
	case json.Number:
		f, err := v.Float64()
		return f, (err == nil), err
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false, err
		}
		return f, true, nil
	default:
		rv := reflect.ValueOf(val)
		switch rv.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return float64(rv.Int()), true, nil
		case reflect.Float32, reflect.Float64:
			return rv.Float(), true, nil
		case reflect.String:
			f, err := strconv.ParseFloat(rv.String(), 64)
			if err != nil {
				return 0, false, err
			}
			return f, true, nil
		}
		return 0, false, fmt.Errorf("cannot convert %T to float64", val)
	}
}

func toFloat(val any) (float64, error) {
	f, _, err := toNumeric(val)
	return f, err
}

func minError(fieldName string, val, min float64) *ValidationError {
	return &ValidationError{
		Field:  fieldName,
		Code:   i18n.CodeSchemaValidationMin,
		Rule:   "min",
		Value:  val,
		Params: map[string]any{"field": fieldName, "min": fmt.Sprint(min), "value": fmt.Sprint(val)},
	}
}

func maxError(fieldName string, val, max float64) *ValidationError {
	return &ValidationError{
		Field:  fieldName,
		Code:   i18n.CodeSchemaValidationMax,
		Rule:   "max",
		Value:  val,
		Params: map[string]any{"field": fieldName, "max": fmt.Sprint(max), "value": fmt.Sprint(val)},
	}
}

func compareError(fieldA, fieldB, relation string) *ValidationError {
	var code i18n.Code
	switch relation {
	case "less than":
		code = i18n.CodeSchemaValidationLtField
	case "greater than":
		code = i18n.CodeSchemaValidationGtField
	case "less than or equal to":
		code = i18n.CodeSchemaValidationLteField
	default:
		code = i18n.CodeSchemaValidationGteField
	}
	return &ValidationError{
		Field:  fmt.Sprintf("%s,%s", fieldA, fieldB),
		Code:   code,
		Rule:   strings.ReplaceAll(relation, " ", "_") + "_field",
		Params: map[string]any{"field": fieldA, "other": fieldB},
	}
}

// hasRule reports whether rules names the rule called name.
func hasRule(rules []domain.ValidationRule, name string) bool {
	for _, r := range rules {
		if r.Rule == name {
			return true
		}
	}
	return false
}
