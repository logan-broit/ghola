package testutil

// NoopQueries implements sqlc.Querier with zero-value returns.
// Embed this in test mocks and override only the methods you need.

import (
	"context"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository/sqlc"

	"github.com/google/uuid"
)

type NoopQueries struct{}

func (NoopQueries) CleanupExpiredAdminSessions(context.Context) error { return nil }
func (NoopQueries) CountActiveAPIKeysByUser(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}
func (NoopQueries) CountActiveAdminSessions(context.Context) (int64, error)                 { return 0, nil }
func (NoopQueries) CountAuditLogs(context.Context, sqlc.CountAuditLogsParams) (int64, error) { return 0, nil }
func (NoopQueries) CountGitCommits(context.Context, uuid.UUID) (int32, error)                { return 0, nil }
func (NoopQueries) CountJournalEntriesByType(context.Context, sqlc.CountJournalEntriesByTypeParams) (int32, error) {
	return 0, nil
}
func (NoopQueries) CountMemoryBlockVersions(context.Context, sqlc.CountMemoryBlockVersionsParams) (int32, error) {
	return 0, nil
}
func (NoopQueries) CountMemoryBlocksByUser(context.Context, uuid.UUID) (int32, error) { return 0, nil }
func (NoopQueries) CountUsers(context.Context) (sqlc.CountUsersRow, error) {
	return sqlc.CountUsersRow{}, nil
}
func (NoopQueries) CreateAPIKey(context.Context, sqlc.CreateAPIKeyParams) (sqlc.ApiKey, error) {
	return sqlc.ApiKey{}, nil
}
func (NoopQueries) CreateAdminSession(context.Context, sqlc.CreateAdminSessionParams) (sqlc.AdminSession, error) {
	return sqlc.AdminSession{}, nil
}
func (NoopQueries) CreateAuditLog(context.Context, sqlc.CreateAuditLogParams) (sqlc.AuditLog, error) {
	return sqlc.AuditLog{}, nil
}
func (NoopQueries) CreateGitCommit(context.Context, sqlc.CreateGitCommitParams) (sqlc.GitCommit, error) {
	return sqlc.GitCommit{}, nil
}
func (NoopQueries) CreateJournalEntry(context.Context, sqlc.CreateJournalEntryParams) (sqlc.Journal, error) {
	return sqlc.Journal{}, nil
}
func (NoopQueries) CreateMemoryBlock(context.Context, sqlc.CreateMemoryBlockParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (NoopQueries) CreateUser(context.Context, sqlc.CreateUserParams) (sqlc.User, error) {
	return sqlc.User{}, nil
}
func (NoopQueries) CreateUserWithPassword(context.Context, sqlc.CreateUserWithPasswordParams) (sqlc.User, error) {
	return sqlc.User{}, nil
}
func (NoopQueries) DeactivateUser(context.Context, uuid.UUID) error   { return nil }
func (NoopQueries) DeleteExpiredMemories(context.Context) error              { return nil }
func (NoopQueries) IncrementRecallCount(context.Context, sqlc.IncrementRecallCountParams) error {
	return nil
}
func (NoopQueries) PruneOldVersions(context.Context, int32) error            { return nil }
func (NoopQueries) DeleteJournalEntry(context.Context, sqlc.DeleteJournalEntryParams) error {
	return nil
}
func (NoopQueries) DeleteMemoryBlock(context.Context, sqlc.DeleteMemoryBlockParams) error {
	return nil
}
func (NoopQueries) DeleteUser(context.Context, uuid.UUID) error { return nil }
func (NoopQueries) ExportMemories(context.Context, uuid.UUID) ([]sqlc.ExportMemoriesRow, error) {
	return nil, nil
}
func (NoopQueries) GetAPIKeyByHash(context.Context, string) (sqlc.GetAPIKeyByHashRow, error) {
	return sqlc.GetAPIKeyByHashRow{}, nil
}
func (NoopQueries) GetAPIKeyByID(context.Context, uuid.UUID) (sqlc.ApiKey, error) {
	return sqlc.ApiKey{}, nil
}
func (NoopQueries) GetAccessibleMemoryBlocks(context.Context, uuid.UUID) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) GetAccessibleMemoryBlocksByType(context.Context, sqlc.GetAccessibleMemoryBlocksByTypeParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) GetAdminSessionByToken(context.Context, string) (sqlc.GetAdminSessionByTokenRow, error) {
	return sqlc.GetAdminSessionByTokenRow{}, nil
}
func (NoopQueries) GetAdminStats(context.Context) (sqlc.GetAdminStatsRow, error) {
	return sqlc.GetAdminStatsRow{}, nil
}
func (NoopQueries) GetAllCurrentMemoryBlocks(context.Context) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) GetAllCurrentMemoryBlocksWithOrg(context.Context) ([]sqlc.GetAllCurrentMemoryBlocksWithOrgRow, error) {
	return nil, nil
}
func (NoopQueries) GetAuditLogsByResource(context.Context, sqlc.GetAuditLogsByResourceParams) ([]sqlc.AuditLog, error) {
	return nil, nil
}
func (NoopQueries) GetCurrentMemoryBlockByName(context.Context, sqlc.GetCurrentMemoryBlockByNameParams) (sqlc.CurrentMemoryBlock, error) {
	return sqlc.CurrentMemoryBlock{}, nil
}
func (NoopQueries) GetCurrentMemoryBlocks(context.Context, uuid.UUID) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) GetCurrentMemoryBlocksByTier(context.Context, sqlc.GetCurrentMemoryBlocksByTierParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) GetCurrentMemoryBlocksByType(context.Context, sqlc.GetCurrentMemoryBlocksByTypeParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) GetGitCommit(context.Context, sqlc.GetGitCommitParams) (sqlc.GitCommit, error) {
	return sqlc.GitCommit{}, nil
}
func (NoopQueries) GetGitCommitByHash(context.Context, sqlc.GetGitCommitByHashParams) (sqlc.GitCommit, error) {
	return sqlc.GitCommit{}, nil
}
func (NoopQueries) GetJournalEntriesByType(context.Context, sqlc.GetJournalEntriesByTypeParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (NoopQueries) GetJournalEntriesByVectorIDs(context.Context, sqlc.GetJournalEntriesByVectorIDsParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (NoopQueries) GetJournalEntry(context.Context, sqlc.GetJournalEntryParams) (sqlc.Journal, error) {
	return sqlc.Journal{}, nil
}
func (NoopQueries) GetJournalEntryByID(context.Context, sqlc.GetJournalEntryByIDParams) (sqlc.Journal, error) {
	return sqlc.Journal{}, nil
}
func (NoopQueries) GetLatestGitCommit(context.Context, uuid.UUID) (sqlc.GitCommit, error) {
	return sqlc.GitCommit{}, nil
}
func (NoopQueries) GetMemoryBlockByGUID(context.Context, sqlc.GetMemoryBlockByGUIDParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (NoopQueries) GetMemoryBlockByID(context.Context, sqlc.GetMemoryBlockByIDParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (NoopQueries) GetMemoryBlockHistory(context.Context, sqlc.GetMemoryBlockHistoryParams) ([]sqlc.MemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) GetMemoryContext(context.Context, uuid.UUID) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) GetMemoryScopeDistribution(context.Context) ([]sqlc.GetMemoryScopeDistributionRow, error) {
	return nil, nil
}
func (NoopQueries) GetMemoryStats(context.Context) (sqlc.GetMemoryStatsRow, error) {
	return sqlc.GetMemoryStatsRow{}, nil
}
func (NoopQueries) GetMemoryTypeDistribution(context.Context) ([]sqlc.GetMemoryTypeDistributionRow, error) {
	return nil, nil
}
func (NoopQueries) GetNextMemoryBlockVersion(context.Context, sqlc.GetNextMemoryBlockVersionParams) (int32, error) {
	return 0, nil
}
func (NoopQueries) GetOrCreateUser(context.Context, sqlc.GetOrCreateUserParams) (sqlc.User, error) {
	return sqlc.User{}, nil
}
func (NoopQueries) GetRecentDecisions(context.Context, sqlc.GetRecentDecisionsParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (NoopQueries) GetRecentSolutions(context.Context, sqlc.GetRecentSolutionsParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (NoopQueries) GetTopTags(context.Context, int32) ([]sqlc.GetTopTagsRow, error) { return nil, nil }
func (NoopQueries) GetUserByID(context.Context, uuid.UUID) (sqlc.User, error) {
	return sqlc.User{}, nil
}
func (NoopQueries) GetUserByUsername(context.Context, string) (sqlc.User, error) {
	return sqlc.User{}, nil
}
func (NoopQueries) GetUserByUsernameForAuth(context.Context, string) (sqlc.User, error) {
	return sqlc.User{}, nil
}
func (NoopQueries) ListAPIKeysByUser(context.Context, uuid.UUID) ([]sqlc.ListAPIKeysByUserRow, error) {
	return nil, nil
}
func (NoopQueries) ListActiveAdminSessions(context.Context) ([]sqlc.ListActiveAdminSessionsRow, error) {
	return nil, nil
}
func (NoopQueries) ListAllAPIKeys(context.Context, sqlc.ListAllAPIKeysParams) ([]sqlc.ListAllAPIKeysRow, error) {
	return nil, nil
}
func (NoopQueries) ListAuditLogs(context.Context, sqlc.ListAuditLogsParams) ([]sqlc.AuditLog, error) {
	return nil, nil
}
func (NoopQueries) ListAuditLogsByDateRange(context.Context, sqlc.ListAuditLogsByDateRangeParams) ([]sqlc.AuditLog, error) {
	return nil, nil
}
func (NoopQueries) ListGitCommits(context.Context, sqlc.ListGitCommitsParams) ([]sqlc.GitCommit, error) {
	return nil, nil
}
func (NoopQueries) ListGitCommitsByDateRange(context.Context, sqlc.ListGitCommitsByDateRangeParams) ([]sqlc.GitCommit, error) {
	return nil, nil
}
func (NoopQueries) ListJournalEntries(context.Context, sqlc.ListJournalEntriesParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (NoopQueries) ListJournalEntriesByDateRange(context.Context, sqlc.ListJournalEntriesByDateRangeParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (NoopQueries) ListUsers(context.Context, sqlc.ListUsersParams) ([]sqlc.User, error) {
	return nil, nil
}
func (NoopQueries) ListUsersAdmin(context.Context, sqlc.ListUsersAdminParams) ([]sqlc.User, error) {
	return nil, nil
}
func (NoopQueries) ReactivateUser(context.Context, uuid.UUID) error          { return nil }
func (NoopQueries) RevokeAPIKey(context.Context, uuid.UUID) error            { return nil }
func (NoopQueries) RevokeAdminSession(context.Context, uuid.UUID) error      { return nil }
func (NoopQueries) RevokeAdminSessionByToken(context.Context, string) error  { return nil }
func (NoopQueries) RevokeAllAPIKeysByUser(context.Context, uuid.UUID) error  { return nil }
func (NoopQueries) RevokeAllAdminSessionsByUser(context.Context, uuid.UUID) error { return nil }
func (NoopQueries) SearchAccessibleMemoryBlocks(context.Context, sqlc.SearchAccessibleMemoryBlocksParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) SearchAccessibleMemoryBlocksByTags(context.Context, sqlc.SearchAccessibleMemoryBlocksByTagsParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) SearchAccessibleMemoryBlocksByType(context.Context, sqlc.SearchAccessibleMemoryBlocksByTypeParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) SearchAccessibleMemoryBlocksByTypeAndTags(context.Context, sqlc.SearchAccessibleMemoryBlocksByTypeAndTagsParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (NoopQueries) SearchJournalFullText(context.Context, sqlc.SearchJournalFullTextParams) ([]sqlc.SearchJournalFullTextRow, error) {
	return nil, nil
}
func (NoopQueries) SetJournalVectorID(context.Context, sqlc.SetJournalVectorIDParams) error {
	return nil
}
func (NoopQueries) SetUserAdmin(context.Context, sqlc.SetUserAdminParams) error    { return nil }
func (NoopQueries) SetUserPassword(context.Context, sqlc.SetUserPasswordParams) error { return nil }
func (NoopQueries) SupersedeJournalEntry(context.Context, sqlc.SupersedeJournalEntryParams) error {
	return nil
}
func (NoopQueries) UpdateAPIKeyLastUsed(context.Context, uuid.UUID) error { return nil }
func (NoopQueries) UpdateJournalEntry(context.Context, sqlc.UpdateJournalEntryParams) (sqlc.Journal, error) {
	return sqlc.Journal{}, nil
}
func (NoopQueries) UpdateMemoryBlock(context.Context, sqlc.UpdateMemoryBlockParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (NoopQueries) UpdateMemoryBlockScope(context.Context, sqlc.UpdateMemoryBlockScopeParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (NoopQueries) UpdateMemoryBlockType(context.Context, sqlc.UpdateMemoryBlockTypeParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (NoopQueries) UpdateUser(context.Context, sqlc.UpdateUserParams) (sqlc.User, error) {
	return sqlc.User{}, nil
}
func (NoopQueries) UpdateUserAdmin(context.Context, sqlc.UpdateUserAdminParams) (sqlc.User, error) {
	return sqlc.User{}, nil
}

var _ sqlc.Querier = NoopQueries{}
