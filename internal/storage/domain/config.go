package domain

import "context"

type ConfigSQLRepository interface {
	GetMetadata(ctx context.Context, key string) (string, error)
	SetMetadata(ctx context.Context, key, value string) error
	SetLocalMetadata(ctx context.Context, key, value string) error
	GetConfig(ctx context.Context, key string) (string, error)
	SetConfig(ctx context.Context, key, value string) error
	GetStatistics(ctx context.Context) (Statistics, error)
}

type ConfigUseCase interface {
	ConfigureContributorMode(ctx context.Context, params ContributorModeParams) error
	ConfigureTeamMode(ctx context.Context, params TeamModeParams) error
	VerifyInit(ctx context.Context) (VerifyResult, error)
}

type Issue struct{}

type BatchCreateOptions struct{}

type Statistics struct{}

type ContributorModeParams struct{}

type TeamModeParams struct{}

type GlobalDatabaseParams struct{}

type ImportResult struct{}

type VerifyResult struct{}
