package network

import "github.com/cambrian-sh/core/internal/testing/fakemodels"

// useFakeModel serves model under id from the gateway's model source, creating a
// fakemodels source on first use. It replaces the pre-ADR-0139 RegisterModelClient:
// the gateway no longer holds clients of its own.
func useFakeModel(gw *SubstrateLLMGateway, id string, model any) *fakemodels.Models {
	gw.mu.RLock()
	fm, _ := gw.models.(*fakemodels.Models)
	gw.mu.RUnlock()
	if fm == nil {
		fm = fakemodels.New()
		gw.SetStepModels(fm)
	}
	return fm.Set(id, model)
}
