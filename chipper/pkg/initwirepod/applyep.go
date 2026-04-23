package initwirepod

import (
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	botsetup "github.com/kercre123/wire-pod/chipper/pkg/wirepod/setup"
)

// applyUseEpToDiskLikeSubmit chính xác 3 bước giống handler /api-chipper/use_ep trước RestartServer.
func applyUseEpToDiskLikeSubmit() {
	vars.ConfigureEscapePodWirePod()
	botsetup.CreateServerConfig()
	vars.WriteConfigToDisk()
	logger.Println("use_ep: apiConfig + server_config written (same as after Submit).")
}

// RunAutoUseEpFirstBootLikeInitialSubmit: giống 100% initial (EP) → gọi apply rồi RestartServer
// như web.go, tránh bật go StartChipper thêm ở StartFromProgramInit.
func RunAutoUseEpFirstBootLikeInitialSubmit() {
	applyUseEpToDiskLikeSubmit()
	RestartServer()
	vars.SetSkipStartChipperAfterAutoUseEp(true)
	logger.Println("auto: first boot — same as initial.html EP Submit (use_ep + RestartServer).")
}
