package domain

import "context"

type DoltRemoteUseCase interface {
	CreateRemote(ctx context.Context, name, url string) error
	UpdateRemote(ctx context.Context, name, url string) error
	DeleteRemote(ctx context.Context, name string) error
	ListRemotes(ctx context.Context) ([]Remote, error)
}

type Remote struct{}
