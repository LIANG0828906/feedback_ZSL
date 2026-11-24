package analyzer

import (
	"spatialir/internal/loader"
	"spatialir/internal/model"
)

// Dataset 是分析器视图，复用 loader.Dataset 的全部本地数据。
type Dataset = loader.Dataset

var _ = model.CodeMeta // 保持 model 依赖显式化
