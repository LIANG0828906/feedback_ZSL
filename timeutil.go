package strokelab

import "time"

var timeNowUTC = func() time.Time { return time.Now().UTC() }
