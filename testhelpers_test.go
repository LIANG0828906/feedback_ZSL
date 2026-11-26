package silhouette

func asDomainError(err error, target **DomainError) bool {
	if err == nil {
		return false
	}
	de, ok := err.(*DomainError)
	if ok {
		*target = de
	}
	return ok
}
