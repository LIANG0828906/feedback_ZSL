package session

import (
	"volumelab/pkg/measure"
	"volumelab/pkg/model"
	"volumelab/pkg/volume"
)

func measureValidate(v *volume.Volume, annotation model.Annotation) *model.AppError {
	return measure.ValidateStoredAnnotation(v, annotation)
}
