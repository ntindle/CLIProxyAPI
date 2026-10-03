package registry

// CloneModelInfo returns an independent copy of a model definition.
func CloneModelInfo(model *ModelInfo) *ModelInfo {
	return cloneModelInfo(model)
}
