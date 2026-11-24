package formats

import (
	"encoding/json"
	"fmt"
	"os"

	"virtualstudio/model"
)

func LoadCommands(path string, scene *model.Scene) (*model.CommandSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.WrapError(model.CodeIO, path, "read commands", err)
	}
	commands, err := ParseCommands(data)
	if err != nil {
		return nil, err
	}
	if err := model.ValidateCommands(scene, commands); err != nil {
		return nil, err
	}
	return commands, nil
}

func ParseCommands(data []byte) (*model.CommandSet, error) {
	var commands model.CommandSet
	if err := json.Unmarshal(data, &commands); err != nil {
		return nil, model.WrapError(model.CodeDecode, "commands", "invalid command JSON", err)
	}
	if commands.Version == "" {
		return nil, model.NewError(model.CodeVersion, "version", "required field is missing")
	}
	if commands.WindowFrames <= 0 {
		return nil, model.NewError(model.CodeVersion, "window_frames", fmt.Sprintf("invalid value %d", commands.WindowFrames))
	}
	return &commands, nil
}
