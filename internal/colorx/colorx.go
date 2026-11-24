package colorx

type Intent struct {
	Name        string
	SourceSpace string
	TargetSpace string
	Reversible  bool
}

var intents = map[string]Intent{
	"identity": {
		Name: "identity", SourceSpace: "*", TargetSpace: "*", Reversible: true,
	},
	"rgb_to_ycbcr_bt601": {
		Name: "rgb_to_ycbcr_bt601", SourceSpace: "RGB", TargetSpace: "YCbCr-BT601", Reversible: true,
	},
	"ycbcr_bt601_to_rgb": {
		Name: "ycbcr_bt601_to_rgb", SourceSpace: "YCbCr-BT601", TargetSpace: "RGB", Reversible: true,
	},
	"rgb_to_ycbcr_bt709": {
		Name: "rgb_to_ycbcr_bt709", SourceSpace: "RGB", TargetSpace: "YCbCr-BT709", Reversible: true,
	},
	"ycbcr_bt709_to_rgb": {
		Name: "ycbcr_bt709_to_rgb", SourceSpace: "YCbCr-BT709", TargetSpace: "RGB", Reversible: true,
	},
}

func KnownIntent(name string) (Intent, bool) {
	in, ok := intents[name]
	return in, ok
}

func Reachable(intentName, source, target string) bool {
	in, ok := intents[intentName]
	if !ok {
		return false
	}
	if in.Name == "identity" {
		return source == target
	}
	return in.SourceSpace == source && in.TargetSpace == target
}
