package workbench

import "storyboardstudio/internal/model"

func SetLastResultForTest(wb *Workbench, projectID string, result *model.RunResult) {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	wb.lastResults[projectID] = result
}
