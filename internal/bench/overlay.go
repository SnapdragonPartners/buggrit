package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"

	"github.com/SnapdragonPartners/buggrit/internal/config"
)

// overlayModelFields are the model fields an overlay may set, under
// models.<stage> or models.default.
func overlayModelFields() []string {
	return []string{"provider", "model", "base_url", "api_key_env", "reasoning_effort", "max_output_tokens", "request_timeout"}
}

// overlayLimitFields are the limits an overlay may set.
func overlayLimitFields() []string { return []string{"pass_deadline", "max_concurrency"} }

// Merge applies an arm's overlay to the base config and returns the result,
// validated as a config. An overlay changes what a model/prompt/harness
// comparison varies (the models, and the pass's deadline and concurrency);
// prompt and harness variants are the arm's ref. Anything else in an
// overlay is an error, and so is a merged config with telemetry off, since
// the results come from telemetry.
func Merge(base, overlay []byte) ([]byte, error) {
	var b map[string]any
	if err := decodeNumbers(base, &b); err != nil {
		return nil, fmt.Errorf("base config: %w", err)
	}
	if b == nil {
		return nil, errors.New("base config: must be a JSON object")
	}
	if len(bytes.TrimSpace(overlay)) > 0 && !bytes.Equal(bytes.TrimSpace(overlay), []byte("null")) {
		var o map[string]any
		if err := decodeNumbers(overlay, &o); err != nil {
			return nil, fmt.Errorf("overlay: %w", err)
		}
		if err := checkOverlay(o); err != nil {
			return nil, err
		}
		mergeInto(b, o)
	}
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the merged config: %w", err)
	}
	cfg, err := config.Parse(out)
	if err != nil {
		return nil, fmt.Errorf("merged config: %w", err)
	}
	if !cfg.Telemetry.Enabled {
		return nil, errors.New("merged config: telemetry is off, and a benchmark's results come from telemetry")
	}
	return append(out, '\n'), nil
}

func decodeNumbers(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err //nolint:wrapcheck // the caller names what it was decoding
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

func checkOverlay(o map[string]any) error {
	var errs []error
	for _, k := range sortedKeys(o) {
		switch k {
		case "models":
			models, ok := o[k].(map[string]any)
			if !ok {
				errs = append(errs, errors.New("overlay: models must be an object"))
				continue
			}
			for _, stage := range sortedKeys(models) {
				if stage != "default" && !slices.Contains(config.Stages(), config.Stage(stage)) {
					errs = append(errs, fmt.Errorf("overlay: models.%s is not a stage", stage))
					continue
				}
				errs = append(errs, onlyFields("models."+stage, models[stage], overlayModelFields())...)
			}
		case "limits":
			errs = append(errs, onlyFields("limits", o[k], overlayLimitFields())...)
		default:
			errs = append(errs, fmt.Errorf("overlay: %s can't be overlaid; an overlay sets only models and limits.%v", k, overlayLimitFields()))
		}
	}
	return errors.Join(errs...) //nolint:wrapcheck // each error says it's the overlay's
}

func onlyFields(field string, v any, allowed []string) []error {
	m, ok := v.(map[string]any)
	if !ok {
		return []error{fmt.Errorf("overlay: %s must be an object", field)}
	}
	var errs []error
	for _, k := range sortedKeys(m) {
		if !slices.Contains(allowed, k) {
			errs = append(errs, fmt.Errorf("overlay: %s.%s can't be overlaid (allowed: %v)", field, k, allowed))
		}
	}
	return errs
}

// mergeInto sets every leaf of o in b, merging objects key by key.
func mergeInto(b, o map[string]any) {
	for k, v := range o {
		if om, ok := v.(map[string]any); ok {
			if bm, ok := b[k].(map[string]any); ok {
				mergeInto(bm, om)
				continue
			}
		}
		b[k] = v
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
