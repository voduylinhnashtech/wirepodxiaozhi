package initwirepod

import (
	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	botsetup "github.com/kercre123/wire-pod/chipper/pkg/wirepod/setup"
)

// applyUseEpToDiskLikeSubmit: phần cấu hình giống handler /api-chipper/use_ep (trước Restart).
func applyUseEpToDiskLikeSubmit() {
	vars.ConfigureEscapePodWirePod()
	botsetup.CreateServerConfig()
	vars.WriteConfigToDisk()
	logger.Println("use_ep: apiConfig + server_config written (same as after Submit).")
}
