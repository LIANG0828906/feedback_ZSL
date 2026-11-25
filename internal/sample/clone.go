package sample

import "coldkernel/internal/model"

// CloneProject returns an independent deep copy so tests can edit one copy
// while keeping the original project untouched.
func CloneProject(p *model.Project) *model.Project {
	cp := *p
	cp.Functions = make([]*model.Function, len(p.Functions))
	for i, f := range p.Functions {
		fc := *f
		cp.Functions[i] = &fc
	}
	cp.Requests = make([]*model.Request, len(p.Requests))
	for i, r := range p.Requests {
		rc := *r
		cp.Requests[i] = &rc
	}
	cp.Index()
	return &cp
}
