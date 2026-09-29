package config

func (c *Config) TerminalOn() bool {
	return c.TerminalEnabled == nil || *c.TerminalEnabled
}
