package workbench

import (
	"encoding/json"
	"errors"

	"storyboardstudio/internal/interop"
	"storyboardstudio/internal/model"
	"storyboardstudio/internal/sample"
)

type NegativeCase struct {
	Name         string `json:"name"`
	ExpectedCode string `json:"expected_code"`
	GotCode      string `json:"got_code"`
	OK           bool   `json:"ok"`
	ConflictNode string `json:"conflict_node,omitempty"`
	Message      string `json:"message,omitempty"`
}

func NegativeChecks() []NegativeCase {
	cases := []NegativeCase{}

	_, err := model.LoadProject([]byte(`{"id":"missing-fields"}`))
	cases = append(cases, negativeCase("missing-schema-field", string(model.CodeSchema), err))

	project := sample.Generate(sample.DefaultSeeds()[0])
	project.Nodes[0].Parents = []string{"missing_node"}
	if err := model.ValidateProject(project); err == nil {
		cases = append(cases, NegativeCase{Name: "dangling-reference", ExpectedCode: string(model.CodeRef), OK: false, Message: "expected error"})
	} else {
		cases = append(cases, negativeCase("dangling-reference", string(model.CodeRef), err))
	}

	cycle := sample.Generate(sample.DefaultSeeds()[0])
	for i := range cycle.Nodes {
		switch cycle.Nodes[i].ID {
		case "n00_world":
			cycle.Nodes[i].Parents = []string{"n01_office"}
		case "n01_office":
			cycle.Nodes[i].Parents = []string{"n00_world"}
		}
	}
	cases = append(cases, negativeCase("directed-cycle", string(model.CodeRef), model.ValidateProject(cycle)))

	badChoice := sample.Generate(sample.DefaultSeeds()[0])
	badChoice.ChoiceSequence.Items[0].OptionID = "does_not_exist"
	data, _ := json.Marshal(badChoice)
	_, err = model.LoadProject(data)
	cases = append(cases, negativeCase("unknown-choice-option", string(model.CodeChoice), err))

	_, _, err = interop.Decode([]byte(`{"schema":"storyboard/9.9"}`))
	cases = append(cases, negativeCase("unsupported-storyboard-version", string(model.CodeVersion), err))

	badDuration := sample.Generate(sample.DefaultSeeds()[0])
	for i := range badDuration.Nodes {
		if len(badDuration.Nodes[i].Shots) > 0 {
			badDuration.Nodes[i].Shots[0].DurationMS = -1
		}
	}
	cases = append(cases, negativeCase("negative-duration", string(model.CodeSchema), model.ValidateProject(badDuration)))

	badAspect := sample.Generate(sample.DefaultSeeds()[0])
	for i := range badAspect.Nodes {
		if len(badAspect.Nodes[i].Shots) > 0 {
			badAspect.Nodes[i].Shots[0].AspectPath = "not-a-legal-path"
		}
	}
	cases = append(cases, negativeCase("illegal-aspect-path", string(model.CodeSchema), model.ValidateProject(badAspect)))

	return cases
}

func negativeCase(name, expected string, err error) NegativeCase {
	result := NegativeCase{Name: name, ExpectedCode: expected}
	if err == nil {
		result.Message = "expected error but got nil"
		return result
	}
	var coded *model.Error
	if errors.As(err, &coded) {
		result.GotCode = string(coded.Code)
		result.ConflictNode = coded.ConflictNode
		result.Message = coded.Message
		result.OK = string(coded.Code) == expected
		return result
	}
	result.Message = err.Error()
	return result
}
