package sample

import "maskpipeline/internal/apperr"

func errorfDecode(op, field, format string, args ...any) *apperr.Error {
	return apperr.New(apperr.CodeDecode, op, format, args...).WithField(field)
}

func errorfMeta(op, field, format string, args ...any) *apperr.Error {
	return apperr.New(apperr.CodeMeta, op, format, args...).WithField(field)
}
