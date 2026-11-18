package relay

import (
	"fmt"
	"time"
)

type Config struct {
	BindIP           string
	ControlUDP       string
	ControlTCP       string
	HTTP             string
	DBPath           string
	PortMin          int
	PortMax          int
	LeaseWindow      time.Duration
	RecoveryCooldown time.Duration
	InFlightDrain    time.Duration
	ProbeTimeout     time.Duration
	ProbeInterval    time.Duration
	SweepInterval    time.Duration
	ForwardUDP       string
	ForwardTCP       string
}

func DefaultConfig() Config {
	return Config{
		BindIP:           "127.0.0.1",
		ControlUDP:       "127.0.0.1:17000",
		ControlTCP:       "127.0.0.1:17001",
		HTTP:             "127.0.0.1:17080",
		DBPath:           "relay.db",
		PortMin:          21000,
		PortMax:          21999,
		LeaseWindow:      8 * time.Second,
		RecoveryCooldown: 3 * time.Second,
		InFlightDrain:    250 * time.Millisecond,
		ProbeTimeout:     180 * time.Millisecond,
		ProbeInterval:    4 * time.Second,
		SweepInterval:    100 * time.Millisecond,
	}
}

func (c Config) Validate() error {
	if c.BindIP == "" {
		return fmt.Errorf("bind_ip is required")
	}
	if c.ControlUDP == "" || c.ControlTCP == "" || c.HTTP == "" {
		return fmt.Errorf("control_udp, control_tcp and http addresses are required")
	}
	if c.DBPath == "" {
		return fmt.Errorf("db_path is required")
	}
	if c.PortMin <= 0 || c.PortMax < c.PortMin || c.PortMax > 65535 {
		return fmt.Errorf("invalid mapping port range %d-%d", c.PortMin, c.PortMax)
	}
	if c.LeaseWindow <= 0 || c.RecoveryCooldown < 0 || c.InFlightDrain < 0 {
		return fmt.Errorf("lease_window must be positive and cooldown/drain must not be negative")
	}
	if c.ProbeTimeout <= 0 || c.ProbeInterval <= 0 || c.SweepInterval <= 0 {
		return fmt.Errorf("probe and sweep intervals must be positive")
	}
	return nil
}
