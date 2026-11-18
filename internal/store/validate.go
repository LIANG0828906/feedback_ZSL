package store

import (
	"fmt"
	"regexp"

	"configcenter/internal/model"
)

const (
	MaxChangesPerPublish = 256
	MaxKeyLength         = 128
	MaxValueLength       = 64 * 1024
)

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func ValidateChanges(changes []model.Change) error {
	if len(changes) == 0 {
		return model.NewAPIError("E_VALIDATION", "at least one configuration change is required", model.ErrValidation.HTTPStatus)
	}
	if len(changes) > MaxChangesPerPublish {
		return model.NewAPIError(
			"E_VALIDATION",
			fmt.Sprintf("too many changes: maximum is %d", MaxChangesPerPublish),
			model.ErrValidation.HTTPStatus,
		)
	}

	seen := make(map[string]struct{}, len(changes))
	for i, change := range changes {
		if change.Key == "" {
			return model.NewAPIError(
				"E_VALIDATION",
				fmt.Sprintf("change %d has an empty key", i),
				model.ErrValidation.HTTPStatus,
			)
		}
		if len(change.Key) > MaxKeyLength {
			return model.NewAPIError(
				"E_VALIDATION",
				fmt.Sprintf("change %q has a key longer than %d bytes", change.Key, MaxKeyLength),
				model.ErrValidation.HTTPStatus,
			)
		}
		if !keyPattern.MatchString(change.Key) {
			return model.NewAPIError(
				"E_VALIDATION",
				fmt.Sprintf("change %q has an invalid key", change.Key),
				model.ErrValidation.HTTPStatus,
			)
		}
		if change.Value == "" {
			return model.NewAPIError(
				"E_VALIDATION",
				fmt.Sprintf("change %q has an empty value", change.Key),
				model.ErrValidation.HTTPStatus,
			)
		}
		if len(change.Value) > MaxValueLength {
			return model.NewAPIError(
				"E_VALIDATION",
				fmt.Sprintf("change %q has a value longer than %d bytes", change.Key, MaxValueLength),
				model.ErrValidation.HTTPStatus,
			)
		}
		if _, ok := seen[change.Key]; ok {
			return model.NewAPIError(
				"E_VALIDATION",
				fmt.Sprintf("change key %q is duplicated", change.Key),
				model.ErrValidation.HTTPStatus,
			)
		}
		seen[change.Key] = struct{}{}
	}
	return nil
}
