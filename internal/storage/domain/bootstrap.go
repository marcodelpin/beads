package domain

import "context"

type BootstrapUseCase interface {
	BootstrapProject(ctx context.Context, params BootstrapProjectParams) (BootstrapProjectResult, error)
}

type BootstrapProjectParams struct{}

type BootstrapProjectResult struct{}
