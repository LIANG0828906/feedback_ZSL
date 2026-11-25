package main

import "soundlab/audiofp"

func asError(err error) (*audiofp.Error, bool) {
	return audiofp.AsError(err)
}
