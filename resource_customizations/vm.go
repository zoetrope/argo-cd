package resource_customizations

import (
	"os"
	"path/filepath"

	"github.com/argoproj/argo-cd/engine/pkg/apis/application/v1alpha1"

	"github.com/argoproj/argo-cd/engine/util/lua"
	"github.com/gobuffalo/packr"
)

const (
	healthScriptFile          = "health.lua"
	actionScriptFile          = "action.lua"
	actionDiscoveryScriptFile = "discovery.lua"
)

var (
	box packr.Box
)

func init() {
	box = packr.NewBox(".")
}

func getPredefinedScript(objKey string, scriptType lua.ScriptType) (string, error) {
	scriptFile := healthScriptFile
	switch scriptType {
	case lua.ScriptTypeHealth:
		scriptFile = healthScriptFile
	case lua.ScriptTypeAction:
		scriptFile = actionScriptFile
	case lua.ScriptTypeActionDiscovery:
		scriptFile = actionDiscoveryScriptFile
	}
	data, err := box.MustBytes(filepath.Join(objKey, scriptFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

func NewLuaVM(resourceOverrides map[string]v1alpha1.ResourceOverride) *lua.VM {
	return NewLuaVMExt(resourceOverrides, false)
}

func NewLuaVMExt(resourceOverrides map[string]v1alpha1.ResourceOverride, useOpenLibs bool) *lua.VM {
	return &lua.VM{
		PredefinedScriptsSource: getPredefinedScript,
		ResourceOverrides:       resourceOverrides,
		UseOpenLibs:             useOpenLibs,
	}
}
