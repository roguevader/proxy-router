package system

import "gitlab.com/TitanInd/proxy/proxy-router-v3/internal/interfaces"

type SystemConfigurator struct {
	backup         *Config
	osConfigurator osConfigurator
	log            interfaces.ILogger
}

func NewConfigurator(osConfigurator osConfigurator, log interfaces.ILogger) *SystemConfigurator {
	return &SystemConfigurator{
		osConfigurator: osConfigurator,
		log:            log,
	}
}

func CreateConfigurator(log interfaces.ILogger) (*SystemConfigurator, error) {
	osConfigurator, err := OSConfiguratorFactory()
	if err != nil {
		return nil, err
	}
	return NewConfigurator(osConfigurator, log), nil
}

func (c *SystemConfigurator) ApplyConfig(cfg *Config) error {
	if c.backup == nil {
		backup, err := c.osConfigurator.GetConfig()
		if err != nil {
			return err
		}
		c.backup = backup
		c.log.Debugf("System config backed up: %+v", c.backup)
	}
	err := c.osConfigurator.ApplyConfig(cfg)
	if err != nil {
		return err
	}
	c.log.Debugf("System config applied: %+v", cfg)
	return nil
}

func (c *SystemConfigurator) RestoreConfig() error {
	err := c.osConfigurator.ApplyConfig(c.backup)
	if err != nil {
		return err
	}
	c.log.Debugf("System config restored: %+v", c.backup)
	return nil
}
