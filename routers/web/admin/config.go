// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2019 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package admin

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	system_model "code.gitea.io/gitea/models/system"
	"code.gitea.io/gitea/modules/cache"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/setting/config"
	"code.gitea.io/gitea/modules/storage"
	"code.gitea.io/gitea/modules/templates"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/services/mailer"

	"gitea.com/go-chi/session"
)

const (
	tplConfig         templates.TplName = "admin/config"
	tplConfigSettings templates.TplName = "admin/config_settings/config_settings"
)

// SendTestMail send test mail to confirm mail service is OK
func SendTestMail(ctx *context.Context) {
	email := ctx.FormString("email")
	// Send a test email to the user's email address and redirect back to Config
	if err := mailer.SendTestMail(email); err != nil {
		ctx.Flash.Error(ctx.Tr("admin.config.test_mail_failed", email, err))
	} else {
		ctx.Flash.Info(ctx.Tr("admin.config.test_mail_sent", email))
	}

	ctx.Redirect(setting.AppSubURL + "/-/admin/config")
}

// TestCache test the cache settings
func TestCache(ctx *context.Context) {
	elapsed, err := cache.Test()
	if err != nil {
		ctx.Flash.Error(ctx.Tr("admin.config.cache_test_failed", err))
	} else {
		if elapsed > cache.SlowCacheThreshold {
			ctx.Flash.Warning(ctx.Tr("admin.config.cache_test_slow", elapsed))
		} else {
			ctx.Flash.Info(ctx.Tr("admin.config.cache_test_succeeded", elapsed))
		}
	}

	ctx.Redirect(setting.AppSubURL + "/-/admin/config")
}

func shadowPasswordKV(cfgItem, splitter string) string {
	fields := strings.Split(cfgItem, splitter)
	for i := range fields {
		if strings.HasPrefix(fields[i], "password=") {
			fields[i] = "password=******"
			break
		}
	}
	return strings.Join(fields, splitter)
}

func shadowURL(provider, cfgItem string) string {
	u, err := url.Parse(cfgItem)
	if err != nil {
		log.Error("Shadowing Password for %v failed: %v", provider, err)
		return cfgItem
	}
	if u.User != nil {
		atIdx := strings.Index(cfgItem, "@")
		if atIdx > 0 {
			colonIdx := strings.LastIndex(cfgItem[:atIdx], ":")
			if colonIdx > 0 {
				return cfgItem[:colonIdx+1] + "******" + cfgItem[atIdx:]
			}
		}
	}
	return cfgItem
}

func shadowPassword(provider, cfgItem string) string {
	switch provider {
	case "redis":
		return shadowPasswordKV(cfgItem, ",")
	case "mysql":
		// root:@tcp(localhost:3306)/macaron?charset=utf8
		atIdx := strings.Index(cfgItem, "@")
		if atIdx > 0 {
			colonIdx := strings.Index(cfgItem[:atIdx], ":")
			if colonIdx > 0 {
				return cfgItem[:colonIdx+1] + "******" + cfgItem[atIdx:]
			}
		}
		return cfgItem
	case "postgres":
		// user=jiahuachen dbname=macaron port=5432 sslmode=disable
		if !strings.HasPrefix(cfgItem, "postgres://") {
			return shadowPasswordKV(cfgItem, " ")
		}
		fallthrough
	case "couchbase":
		return shadowURL(provider, cfgItem)
		// postgres://pqgotest:password@localhost/pqgotest?sslmode=verify-full
		// Notice: use shadowURL
	}
	return cfgItem
}

// Config show admin config page
func Config(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("admin.config_summary")
	ctx.Data["PageIsAdminConfig"] = true
	ctx.Data["PageIsAdminConfigSummary"] = true

	ctx.Data["CustomConf"] = setting.CustomConf
	ctx.Data["AppUrl"] = setting.AppURL
	ctx.Data["AppBuiltWith"] = setting.AppBuiltWith
	ctx.Data["Domain"] = setting.Domain
	ctx.Data["RunUser"] = setting.RunUser
	ctx.Data["RunMode"] = util.ToTitleCase(setting.RunMode)
	ctx.Data["GitVersion"] = git.DefaultFeatures().VersionInfo()

	ctx.Data["AppDataPath"] = setting.AppDataPath
	ctx.Data["RepoRootPath"] = setting.RepoRootPath
	ctx.Data["CustomRootPath"] = setting.CustomPath
	ctx.Data["LogRootPath"] = setting.Log.RootPath
	ctx.Data["ScriptType"] = setting.ScriptType
	ctx.Data["ReverseProxyAuthUser"] = setting.ReverseProxyAuthUser
	ctx.Data["ReverseProxyAuthEmail"] = setting.ReverseProxyAuthEmail

	ctx.Data["SSH"] = setting.SSH
	ctx.Data["LFS"] = setting.LFS

	ctx.Data["Service"] = setting.Service
	ctx.Data["DbCfg"] = setting.Database
	ctx.Data["Webhook"] = setting.Webhook
	ctx.Data["MailerEnabled"] = false
	if setting.MailService != nil {
		ctx.Data["MailerEnabled"] = true
		ctx.Data["Mailer"] = setting.MailService
	}

	ctx.Data["CacheAdapter"] = setting.CacheService.Adapter
	ctx.Data["CacheInterval"] = setting.CacheService.Interval

	ctx.Data["CacheConn"] = shadowPassword(setting.CacheService.Adapter, setting.CacheService.Conn)
	ctx.Data["CacheItemTTL"] = setting.CacheService.TTL

	sessionCfg := setting.SessionConfig
	if sessionCfg.Provider == "VirtualSession" {
		var realSession session.Options
		if err := json.Unmarshal([]byte(sessionCfg.ProviderConfig), &realSession); err != nil {
			log.Error("Unable to unmarshall session config for virtual provider config: %s\nError: %v", sessionCfg.ProviderConfig, err)
		}
		sessionCfg.Provider = realSession.Provider
		sessionCfg.ProviderConfig = realSession.ProviderConfig
		sessionCfg.CookieName = realSession.CookieName
		sessionCfg.CookiePath = realSession.CookiePath
		sessionCfg.Gclifetime = realSession.Gclifetime
		sessionCfg.Maxlifetime = realSession.Maxlifetime
		sessionCfg.Secure = realSession.Secure
		sessionCfg.Domain = realSession.Domain
	}
	sessionCfg.ProviderConfig = shadowPassword(sessionCfg.Provider, sessionCfg.ProviderConfig)
	ctx.Data["SessionConfig"] = sessionCfg

	ctx.Data["Git"] = setting.Git
	ctx.Data["AccessLogTemplate"] = setting.Log.AccessLogTemplate
	ctx.Data["LogSQL"] = setting.Database.LogSQL

	ctx.Data["Loggers"] = log.GetManager().DumpLoggers()
	config.GetDynGetter().InvalidateCache()
	prepareStartupProblemsAlert(ctx)

	ctx.HTML(http.StatusOK, tplConfig)
}

func ConfigSettings(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("admin.config_settings")
	ctx.Data["PageIsAdminConfig"] = true
	ctx.Data["PageIsAdminConfigSettings"] = true
	ctx.Data["Backup"] = setting.Backup
	ctx.HTML(http.StatusOK, tplConfigSettings)
}

func validateConfigKeyValue(dynKey, input string) error {
	opt := config.GetConfigOption(dynKey)
	if opt == nil {
		return util.NewInvalidArgumentErrorf("unknown config key: %s", dynKey)
	}

	const limit = 64 * 1024
	if len(input) > limit {
		return util.NewInvalidArgumentErrorf("value length exceeds limit of %d", limit)
	}

	if !json.Valid([]byte(input)) {
		return util.NewInvalidArgumentErrorf("invalid json value for key: %s", dynKey)
	}
	return nil
}

func ChangeConfig(ctx *context.Context) {
	_ = ctx.Req.ParseForm()
	configKeys := ctx.Req.Form["key"]
	configValues := ctx.Req.Form["value"]
	configSettings := map[string]string{}
loop:
	for i, key := range configKeys {
		if i >= len(configValues) {
			ctx.JSONError(ctx.Tr("admin.config.set_setting_failed", key))
			break loop
		}
		value := configValues[i]

		err := validateConfigKeyValue(key, value)
		if err != nil {
			if errors.Is(err, util.ErrInvalidArgument) {
				ctx.JSONError(err.Error())
			} else {
				ctx.JSONError(ctx.Tr("admin.config.set_setting_failed", key))
			}
			break loop
		}
		configSettings[key] = value
	}
	if ctx.Written() {
		return
	}
	if err := system_model.SetSettings(ctx, configSettings); err != nil {
		ctx.ServerError("SetSettings", err)
		return
	}
	config.GetDynGetter().InvalidateCache()

	// 如果备份配置有变更，同步到 app.ini 并重新初始化存储
	backupChanged := false
	for _, key := range configKeys {
		if strings.HasPrefix(key, "backup.") {
			backupChanged = true
			break
		}
	}
	if backupChanged {
		if err := syncBackupConfigToIni(ctx); err != nil {
			log.Error("Failed to sync backup config to app.ini: %v", err)
		}
	}

	ctx.JSONOK()
}

// syncBackupConfigToIni 将动态备份配置同步到 app.ini，然后重新加载并初始化存储
func syncBackupConfigToIni(ctx *context.Context) error {
	cfg, err := setting.NewConfigProviderFromFile(setting.CustomConf)
	if err != nil {
		return err
	}

	backupCfg := setting.Config().Backup
	storageConfig := backupCfg.StorageConfig.Value(ctx)

	sec := cfg.Section("backup")

	// 存储类型
	sec.Key("STORAGE_TYPE").SetValue(string(storageConfig.StorageType))

	// 本地路径
	if storageConfig.StorageType == "local" {
		sec.Key("PATH").SetValue(storageConfig.LocalPath)
	} else {
		sec.Key("PATH").SetValue("")
	}

	// WebDAV 配置
	sec.Key("WEBDAV_URL").SetValue(storageConfig.WebDAVURL)
	sec.Key("WEBDAV_USERNAME").SetValue(storageConfig.WebDAVUsername)
	// 只有密码非空才写入（避免覆盖为空值）
	if storageConfig.WebDAVPassword != "" {
		sec.Key("WEBDAV_PASSWORD").SetValue(storageConfig.WebDAVPassword)
	}

	// 其他选项
	sec.Key("BACKUP_FORMAT").SetValue(backupCfg.Format.Value(ctx))
	sec.Key("SKIP_LFS").SetValue(strconv.FormatBool(backupCfg.SkipLFS.Value(ctx)))
	sec.Key("SKIP_ATTACHMENTS").SetValue(strconv.FormatBool(backupCfg.SkipAttach.Value(ctx)))
	sec.Key("SKIP_PACKAGES").SetValue(strconv.FormatBool(backupCfg.SkipPackages.Value(ctx)))
	sec.Key("SKIP_DB").SetValue(strconv.FormatBool(backupCfg.SkipDB.Value(ctx)))

	if err := cfg.SaveTo(setting.CustomConf); err != nil {
		return err
	}

	// 重新加载配置
	setting.InitCfgProvider(setting.CustomConf)
	setting.LoadCommonSettings()
	setting.LoadSettings()

	// 重新初始化备份存储
	return storage.InitBackup()
}

// TestBackupStorage 测试备份存储连接
func TestBackupStorage(ctx *context.Context) {
	// 先同步配置
	if err := syncBackupConfigToIni(ctx); err != nil {
		ctx.JSONError(ctx.Tr("admin.config.backup_test_failed", err.Error()))
		return
	}

	// 测试存储连接
	if storage.IsDiscardStorage(storage.Backup) {
		ctx.JSONError(ctx.Tr("admin.config.backup_test_failed", "storage not initialized"))
		return
	}

	// 尝试列出对象来验证连接
	if err := storage.Backup.IterateObjects("", func(_ string, _ storage.Object) error {
		return nil
	}); err != nil {
		ctx.JSONError(ctx.Tr("admin.config.backup_test_failed", err.Error()))
		return
	}

	ctx.Flash.Success(ctx.Tr("admin.config.backup_test_success"))
	ctx.JSONOK()
}
