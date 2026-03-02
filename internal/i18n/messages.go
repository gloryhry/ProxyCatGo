package i18n

var messages = map[string]map[string]string{
	"cn": {
		"invalid_token":                     "无效的访问令牌",
		"no_proxy":                          "无代理",
		"switch_success":                    "代理切换成功",
		"service_start_success":             "服务启动成功",
		"service_stop_success":              "服务停止成功",
		"service_restart_success":           "服务重启成功",
		"service_not_running":               "服务未在运行",
		"service_already_running":           "服务已在运行",
		"invalid_action":                    "无效的操作",
		"operation_failed":                  "操作失败: {}",
		"proxy_save_success":                "代理保存成功",
		"proxy_save_failed":                 "代理保存失败: {}",
		"ip_list_save_success":              "IP名单保存成功",
		"ip_list_save_failed":               "IP名单保存失败: {}",
		"logs_cleared":                      "日志已清除",
		"clear_logs_failed":                 "清除日志失败: {}",
		"users_save_success":                "用户保存成功",
		"users_save_failed":                 "用户保存失败：{}",
		"unsupported_language":              "不支持的语言",
		"proxy_check_result":                "代理检查完成，有效代理：{}个",
		"proxy_check_failed":                "代理检查失败: {}",
		"proxy_refresh_success":             "代理刷新成功",
		"proxy_refresh_failed":              "代理刷新失败: {}",
		"proxy_refresh_skipped":             "代理刷新已跳过",
		"health_check_mode_basic":           "基础检测",
		"health_check_mode_traffic":         "流量模拟检测",
		"health_reason_connect_failed":      "连接失败",
		"health_reason_tls_failed":          "TLS握手失败",
		"health_reason_below_success_ratio": "成功率不足",
		"version_info_not_found":            "未找到版本信息",
		"update_check_error":                "检查更新失败: {}",
	},
	"en": {
		"invalid_token":                     "Invalid access token",
		"no_proxy":                          "No proxy",
		"switch_success":                    "Proxy switched successfully",
		"service_start_success":             "Service started successfully",
		"service_stop_success":              "Service stopped successfully",
		"service_restart_success":           "Service restarted successfully",
		"service_not_running":               "Service is not running",
		"service_already_running":           "Service is already running",
		"invalid_action":                    "Invalid action",
		"operation_failed":                  "Operation failed: {}",
		"proxy_save_success":                "Proxy saved successfully",
		"proxy_save_failed":                 "Failed to save proxy: {}",
		"ip_list_save_success":              "IP list saved successfully",
		"ip_list_save_failed":               "Failed to save IP list: {}",
		"logs_cleared":                      "Logs cleared",
		"clear_logs_failed":                 "Failed to clear logs: {}",
		"users_save_success":                "Users saved successfully",
		"users_save_failed":                 "Failed to save users: {}",
		"unsupported_language":              "Unsupported language",
		"proxy_check_result":                "Proxy check completed, valid proxies: {}",
		"proxy_check_failed":                "Proxy check failed: {}",
		"proxy_refresh_success":             "Proxy refresh completed",
		"proxy_refresh_failed":              "Proxy refresh failed: {}",
		"proxy_refresh_skipped":             "Proxy refresh skipped",
		"health_check_mode_basic":           "Basic health check",
		"health_check_mode_traffic":         "Traffic simulation health check",
		"health_reason_connect_failed":      "Connection failed",
		"health_reason_tls_failed":          "TLS handshake failed",
		"health_reason_below_success_ratio": "Below success ratio",
		"version_info_not_found":            "Version information not found",
		"update_check_error":                "Failed to check for updates: {}",
	},
}

func Get(key, lang string, args ...string) string {
	if lang == "" {
		lang = "cn"
	}

	langDict, ok := messages[lang]
	if !ok {
		langDict = messages["cn"]
	}

	msg, ok := langDict[key]
	if !ok {
		if fallback, fallbackOK := messages["cn"][key]; fallbackOK {
			msg = fallback
		} else {
			msg = key
		}
	}

	for _, arg := range args {
		msg = replaceOne(msg, "{}", arg)
	}
	return msg
}

func replaceOne(s, old, new string) string {
	idx := -1
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			idx = i
			break
		}
	}
	if idx < 0 {
		return s
	}
	return s[:idx] + new + s[idx+len(old):]
}
