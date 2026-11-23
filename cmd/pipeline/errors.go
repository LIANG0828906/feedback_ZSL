package main

import "vqa/internal/errs"

func errInvalidMode(mode string) error {
	return errs.New(errs.EPara, "unknown run mode").With("mode", mode)
}
