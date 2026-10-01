package api

import (
	"net/http"

	"vault/internal/apperr"
	"vault/internal/config"
	"vault/internal/domain"
)

// configFieldView 是全量配置表里的一行（界面直接渲染）。
//
// 目的（用户诉求）：让用户**看到有哪些可配置项 + 每项的含义**，能在页面上改的直接改，
// 不能改的（Editable=false）置灰并展示 EditableReason。
type configFieldView struct {
	// Key 点分路径，与配置文件缩进路径一致（便于对照 config.yaml）。
	Key string `json:"key"`
	// Section 所属配置段（server / http / database / storage / platform / client_compat / log / security / update）。
	Section string `json:"section"`
	// Kind 值类型（string/int/float/bool/duration/list/secret），决定界面控件。
	Kind string `json:"kind"`
	// Value 当前生效值（敏感项只回 ***）。
	Value string `json:"value"`
	// Default 默认值。
	Default string `json:"default"`
	// Desc 说明。
	Desc string `json:"desc"`
	// Editable 是否允许在管理页修改。
	Editable bool `json:"editable"`
	// EditableReason 不可编辑的原因（界面置灰时展示）。
	EditableReason string `json:"editable_reason,omitempty"`
	// NeedsRestart 改完需重启进程才生效（保存仍会落盘）。
	NeedsRestart bool `json:"needs_restart"`
	// Sensitive 敏感项：值已脱敏，且不接受修改。
	Sensitive bool `json:"sensitive"`
	// Source 当前值来源：env（环境变量覆盖，改文件不生效）/ default / file。
	Source string `json:"source"`
}

// configResponse 是全量配置表的响应。
type configResponse struct {
	// Path 配置文件路径（界面展示"改的是哪个文件"）。
	Path string `json:"path"`
	// Warning 保存的副作用提示（写回会丢失注释），界面在保存前展示。
	Warning string `json:"warning,omitempty"`
	// RestartRequired 本次改动中是否包含"需重启才生效"的项。
	RestartRequired bool              `json:"restart_required"`
	Fields          []configFieldView `json:"fields"`
}

// configSaveWarning 明确告知写回的副作用：整份 YAML 重新序列化会丢失注释与字段顺序。
//
// 之所以不做"局部改写"：yaml Node 级改写要维护两套结构同步，风险远大于收益；
// 备份 + 明确告知比"悄悄写坏注释"可控得多（见 config.Save）。
const configSaveWarning = "保存会把配置整体重新序列化写回文件：文件里的注释与字段顺序会丢失（会自动备份为 <配置文件>.bak），可对照 config.example.yaml 恢复注释。"

// configPath 返回被监听的配置文件路径。
func (r *Router) configPath() string {
	if r.deps.Cfg != nil {
		return r.deps.Cfg.Path()
	}
	return ""
}

// configFields 按当前生效配置渲染全量配置表。
func (r *Router) configFields(raw config.Config) []configFieldView {
	defs := config.DescribeFields()
	out := make([]configFieldView, 0, len(defs))
	for _, def := range defs {
		out = append(out, configFieldView{
			Key:            def.Key,
			Section:        def.Section,
			Kind:           string(def.Kind),
			Value:          def.String(&raw),
			Default:        def.DefaultString(),
			Desc:           def.Desc,
			Editable:       def.Editable,
			EditableReason: def.EditableReason,
			NeedsRestart:   def.NeedsRestart,
			Sensitive:      def.Sensitive,
			Source:         def.Source(&raw),
		})
	}
	return out
}

// handleGetConfig 返回全量配置表（只读，不落盘）。
func (r *Router) handleGetConfig(w http.ResponseWriter, req *http.Request) {
	r.writeJSON(w, http.StatusOK, configResponse{
		Path:   r.configPath(),
		Fields: r.configFields(r.deps.App.RawConfig()),
	})
}

// patchConfigRequest 是配置修改请求：只接受"允许在线编辑"的键。
type patchConfigRequest struct {
	// Settings 键 → 值的字符串形式（值按 kind 解析，列表用逗号分隔）。
	Settings map[string]string `json:"settings"`
}

// handlePatchConfig 在管理页修改配置并写回配置文件。
//
// 语义：
//   - 只接受 Editable=true 的键，其余一律 403（reason=config_not_editable）；
//   - 先校验全部键再落盘，避免"改了一半才报错"留下半份配置；
//   - 写回前跑一遍配置校验（config.Save 内部），绝不让一份跑不起来的配置进文件。
func (r *Router) handlePatchConfig(w http.ResponseWriter, req *http.Request) {
	var in patchConfigRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	if len(in.Settings) == 0 {
		r.writeError(w, req, apperr.InvalidParam("settings"))
		return
	}

	// ① 先全部校验（键存在 + 允许编辑），确保不会出现"改一半"。
	for key := range in.Settings {
		def, ok := config.FindField(key)
		if !ok {
			r.writeError(w, req, apperr.InvalidParam("settings."+key).WithArg("reason", "unknown_key"))
			return
		}
		if !def.Editable {
			r.writeError(w, req, apperr.AuthForbidden().WithArg("reason", "config_not_editable").WithArg("key", key))
			return
		}
	}

	// ② 在当前生效配置的**副本**上应用修改（不碰运行中的那份），再校验并落盘。
	raw := r.deps.App.RawConfig()
	restartRequired := false
	for key, value := range in.Settings {
		def, _ := config.FindField(key)
		if err := def.Set(&raw, value); err != nil {
			r.writeError(w, req, apperr.InvalidParam("settings."+key).WithArg("reason", err.Error()))
			return
		}
		if def.NeedsRestart {
			restartRequired = true
		}
	}

	path := r.configPath()
	if err := config.Save(path, &raw); err != nil {
		r.writeError(w, req, apperr.InvalidParam("config").WithArg("reason", err.Error()))
		return
	}

	// ③ 审计（值是运维自己填的，不含密钥：敏感项早在 ① 就被拒了）。
	if p, err := r.principal(req); err == nil {
		for key, value := range in.Settings {
			r.deps.App.Audit(req.Context(), p.UserID, "system.config.patch", key,
				"value="+value, domain.AuditResultOK)
		}
	}

	r.writeJSON(w, http.StatusOK, configResponse{
		Path:            path,
		Warning:         configSaveWarning,
		RestartRequired: restartRequired,
		Fields:          r.configFields(raw),
	})
}
