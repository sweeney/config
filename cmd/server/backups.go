package main

import (
	"fmt"

	"github.com/sweeney/config/internal/config"
	"github.com/sweeney/identity/common/cli"
)

func backupCfg() (cli.BackupConfig, error) {
	cfg, err := config.LoadConfigSvc()
	if err != nil {
		return cli.BackupConfig{}, fmt.Errorf("config: %w", err)
	}
	return cli.BackupConfig{
		ServiceName:       "config",
		DBPath:            cfg.DBPath,
		Env:               string(cfg.Env),
		R2AccountID:       cfg.R2AccountID,
		R2AccessKeyID:     cfg.R2AccessKeyID,
		R2SecretAccessKey: cfg.R2SecretAccessKey,
		R2BucketName:      cfg.R2BucketName,
	}, nil
}

func listBackups() error {
	bcfg, err := backupCfg()
	if err != nil {
		return err
	}
	return cli.ListBackups(bcfg)
}

func restoreBackup(key string) error {
	bcfg, err := backupCfg()
	if err != nil {
		return err
	}
	return cli.RestoreBackup(bcfg, key)
}
