package schemax

import (
	"iconflow/convert"
	"iconflow/errs"
	"iconflow/model"
)

func permilleToPermyriad(v int, field string) (int, model.ConversionRecord, *errs.Error) {
	return convert.PermilleToPermyriad(v, field)
}

func permyriadToPermille(v int, rounding, field string) (int, model.ConversionRecord, *errs.Error) {
	return convert.PermyriadToPermille(v, rounding, field)
}
