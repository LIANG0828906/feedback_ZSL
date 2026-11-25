package main

import (
	"context"

	"soundlab/audiofp"
)

func verifySnapshot(manifestPath, dbPath, projectID string) (audiofp.VerifyReport, error) {
	project, err := audiofp.LoadProject(manifestPath)
	if err != nil {
		return audiofp.VerifyReport{}, err
	}
	final, recipe, _, err := project.ApplyEdits()
	if err != nil {
		return audiofp.VerifyReport{}, err
	}
	if err := project.ValidateTruthAgainstFinal(final, recipe); err != nil {
		return audiofp.VerifyReport{}, err
	}
	store, err := audiofp.OpenStore(dbPath)
	if err != nil {
		return audiofp.VerifyReport{}, err
	}
	defer store.Close()
	return audiofp.VerifyPersistedSnapshot(context.Background(), store, projectID, recipe, project.Manifest.Truth)
}
