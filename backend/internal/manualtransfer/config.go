package manualtransfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

type TaskAccountPage struct {
	TaskID            uint                `json:"task_id"`
	Revision          int64               `json:"revision"`
	AccountIDs        []uint              `json:"account_ids"`
	OrderedAccountIDs []uint              `json:"ordered_account_ids"`
	Accounts          []ManualTaskAccount `json:"accounts"`
}

type UpdateTaskAccountsRequest struct {
	TaskID           uint
	AccountIDs       []uint
	ExpectedRevision int64
	IdempotencyKey   string
	ActorIdentity    string
	ActorType        string
}

type UpdateTaskAccountsResult struct {
	Page     TaskAccountPage
	Existing bool
}

func (s *Service) ListTaskAccounts(taskID uint) (TaskAccountPage, error) {
	var task models.Task
	if err := s.DB.First(&task, taskID).Error; err != nil {
		return TaskAccountPage{}, err
	}
	if err := requireManualTask(&task); err != nil {
		return TaskAccountPage{}, err
	}
	var accounts []ManualTaskAccount
	if err := s.DB.Where("task_id = ?", taskID).Order("position ASC, id ASC").Find(&accounts).Error; err != nil {
		return TaskAccountPage{}, err
	}
	ids := make([]uint, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.AccountID)
	}
	return TaskAccountPage{TaskID: taskID, Revision: normalizedManualAccountRevision(task.ManualAccountRevision), AccountIDs: ids, OrderedAccountIDs: append([]uint(nil), ids...), Accounts: accounts}, nil
}

func (s *Service) UpdateTaskAccounts(ctx context.Context, request UpdateTaskAccountsRequest) (UpdateTaskAccountsResult, error) {
	if s == nil || s.DB == nil {
		return UpdateTaskAccountsResult{}, errors.New("manual transfer database is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if request.TaskID == 0 {
		return UpdateTaskAccountsResult{}, errors.New("task id is required")
	}
	if len(request.AccountIDs) > ManualMaxAccountInputs {
		return UpdateTaskAccountsResult{}, fmt.Errorf("account input count exceeds the technical maximum of %d", ManualMaxAccountInputs)
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" {
		return UpdateTaskAccountsResult{}, errors.New("idempotency key is required")
	}
	if len(request.IdempotencyKey) > 256 || strings.ContainsAny(request.IdempotencyKey, "\x00\r\n") {
		return UpdateTaskAccountsResult{}, errors.New("idempotency key is required")
	}
	if request.ExpectedRevision <= 0 {
		return UpdateTaskAccountsResult{}, ErrRevisionConflict
	}

	mutate := func(task *models.Task) error {
		return s.updateTaskAccountsUnderFence(task, request)
	}
	if s.TaskFence != nil {
		if err := s.TaskFence.WithTaskExclusive(ctx, request.TaskID, mutate); err != nil {
			return UpdateTaskAccountsResult{}, err
		}
	} else {
		var task models.Task
		if err := s.DB.First(&task, request.TaskID).Error; err != nil {
			return UpdateTaskAccountsResult{}, err
		}
		if err := mutate(&task); err != nil {
			return UpdateTaskAccountsResult{}, err
		}
	}
	page, err := s.ListTaskAccounts(request.TaskID)
	if err != nil {
		return UpdateTaskAccountsResult{}, err
	}
	return UpdateTaskAccountsResult{Page: page}, nil
}

func (s *Service) updateTaskAccountsUnderFence(task *models.Task, request UpdateTaskAccountsRequest) error {
	if task == nil || task.ID == 0 {
		return gorm.ErrRecordNotFound
	}
	if err := requireManualTask(task); err != nil {
		return err
	}
	currentRevision := normalizedManualAccountRevision(task.ManualAccountRevision)
	accounts, accountFingerprint, err := s.canonicalTaskAccounts(task, request.AccountIDs)
	if err != nil {
		return err
	}
	fingerprint := fingerprintBytes(fmt.Sprintf("%d\x00%s", request.ExpectedRevision, accountFingerprint))
	var existingKey models.Task
	lookup := s.DB.Where("id = ? AND manual_account_idempotency_key = ?", task.ID, request.IdempotencyKey).First(&existingKey)
	if lookup.Error == nil {
		if existingKey.ManualAccountFingerprint != fingerprint {
			return ErrIdempotencyConflict
		}
		return nil
	}
	if !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
		return lookup.Error
	}
	if request.ExpectedRevision != currentRevision {
		return ErrRevisionConflict
	}

	return s.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.Task{}).Where("id = ? AND manual_account_revision = ?", task.ID, currentRevision).Updates(map[string]interface{}{
			"manual_account_revision":        gorm.Expr("manual_account_revision + 1"),
			"manual_account_idempotency_key": request.IdempotencyKey,
			"manual_account_fingerprint":     fingerprint,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevisionConflict
		}
		if err := tx.Where("task_id = ?", task.ID).Delete(&ManualTaskAccount{}).Error; err != nil {
			return err
		}
		for _, account := range accounts {
			if err := tx.Create(&account).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) canonicalTaskAccounts(task *models.Task, accountIDs []uint) ([]ManualTaskAccount, string, error) {
	if len(accountIDs) > ManualMaxAccountInputs {
		return nil, "", fmt.Errorf("account input count exceeds the technical maximum of %d", ManualMaxAccountInputs)
	}
	configIdentity, err := canonicalTaskConfig(task)
	if err != nil {
		return nil, "", err
	}
	seen := make(map[uint]struct{}, len(accountIDs))
	seenIdentities := make(map[string]struct{}, len(accountIDs))
	seenConfigRemotes := make(map[string]struct{}, len(accountIDs))
	accounts := make([]ManualTaskAccount, 0, len(accountIDs))
	for position, accountID := range accountIDs {
		if accountID == 0 {
			return nil, "", fmt.Errorf("account at position %d must identify a durable quota account", position)
		}
		if _, ok := seen[accountID]; ok {
			return nil, "", fmt.Errorf("duplicate account id %d", accountID)
		}
		seen[accountID] = struct{}{}
		var account models.QuotaAccount
		if err := s.DB.First(&account, accountID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, "", ErrAccountNotFound
			}
			return nil, "", err
		}
		if !account.Enabled {
			return nil, "", fmt.Errorf("account %d is disabled", accountID)
		}
		identity := strings.TrimSpace(account.QuotaKey)
		remote := strings.TrimSpace(account.RemoteName)
		config := strings.TrimSpace(account.ConfigIdentity)
		if config == "" {
			config = configIdentity
		}
		if identity == "" || remote == "" || config == "" {
			return nil, "", fmt.Errorf("account %d has incomplete trusted identity", accountID)
		}
		if len(identity) > manualStringLimit || len(remote) > manualStringLimit || len(config) > manualStringLimit || strings.ContainsAny(identity+remote+config, "\x00\r\n") {
			return nil, "", fmt.Errorf("account %d has an invalid trusted identity", accountID)
		}
		if _, exists := seenIdentities[identity]; exists {
			return nil, "", fmt.Errorf("duplicate account identity %q", identity)
		}
		configRemote := config + "\x00" + remote
		if _, exists := seenConfigRemotes[configRemote]; exists {
			return nil, "", fmt.Errorf("duplicate account config and remote pair %q/%q", config, remote)
		}
		seenIdentities[identity] = struct{}{}
		seenConfigRemotes[configRemote] = struct{}{}
		accounts = append(accounts, ManualTaskAccount{TaskID: task.ID, Position: position, AccountID: account.ID, AccountIdentity: identity, RemoteName: remote, ConfigIdentity: config, Enabled: true})
	}
	encoded, err := json.Marshal(accounts)
	if err != nil {
		return nil, "", err
	}
	return accounts, fingerprintBytes(string(encoded)), nil
}

func (s *Service) resolveSelectedAccounts(taskID uint, inputs []AccountInput, defaultConfig string) ([]frozenAccount, error) {
	if !s.DB.Migrator().HasTable(&ManualTaskAccount{}) {
		return s.resolveAccounts(inputs, defaultConfig)
	}
	var configured []ManualTaskAccount
	if err := s.DB.Where("task_id = ?", taskID).Order("position ASC, id ASC").Find(&configured).Error; err != nil {
		return nil, err
	}
	if len(configured) == 0 {
		return s.resolveAccounts(inputs, defaultConfig)
	}
	if len(inputs) == 0 {
		inputs = make([]AccountInput, 0, len(configured))
		for _, account := range configured {
			inputs = append(inputs, AccountInput{AccountID: account.AccountID})
		}
	}
	byID := make(map[uint]ManualTaskAccount, len(configured))
	for _, account := range configured {
		byID[account.AccountID] = account
	}
	selected := make([]frozenAccount, 0, len(inputs))
	seen := make(map[uint]struct{}, len(inputs))
	for position, input := range inputs {
		accountID := input.AccountID
		if accountID == 0 {
			accountID = input.ID
		}
		account, ok := byID[accountID]
		if !ok || !account.Enabled {
			return nil, ErrAccountNotFound
		}
		var durable models.QuotaAccount
		if err := s.DB.First(&durable, account.AccountID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrAccountNotFound
			}
			return nil, err
		}
		if !durable.Enabled {
			return nil, fmt.Errorf("account %d is disabled", account.AccountID)
		}
		if _, duplicate := seen[accountID]; duplicate {
			return nil, fmt.Errorf("duplicate account id %d", accountID)
		}
		seen[accountID] = struct{}{}
		identity := strings.TrimSpace(durable.QuotaKey)
		remote := strings.TrimSpace(durable.RemoteName)
		config := strings.TrimSpace(durable.ConfigIdentity)
		if config == "" {
			config = defaultConfig
		}
		if identity == "" || remote == "" || config == "" {
			return nil, fmt.Errorf("account %d has incomplete trusted identity", account.AccountID)
		}
		selected = append(selected, frozenAccount{Position: position, AccountID: account.AccountID, AccountIdentity: identity, RemoteName: remote, ConfigIdentity: config})
	}
	return selected, nil
}

func normalizedManualAccountRevision(value int64) int64 {
	if value < 1 {
		return 1
	}
	return value
}

func (s *Service) TaskAccountRevision(taskID uint) (int64, error) {
	var task models.Task
	if err := s.DB.First(&task, taskID).Error; err != nil {
		return 0, err
	}
	if err := requireManualTask(&task); err != nil {
		return 0, err
	}
	return normalizedManualAccountRevision(task.ManualAccountRevision), nil
}

func (s *Service) accountConfigForRun(runID uint) ([]ManualRunAccount, error) {
	var accounts []ManualRunAccount
	if err := s.DB.Where("run_id = ?", runID).Order("position ASC, id ASC").Find(&accounts).Error; err != nil {
		return nil, err
	}
	return accounts, nil
}
