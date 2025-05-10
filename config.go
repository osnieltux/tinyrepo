package main

import "fmt"


type Config struct {
	Server struct {
		Source []string
	}
	Destination struct {
		Arch []string
		Path string
		Packages []string
	}
	Proxy struct {
		Use  bool
		Host string
		Port int
	}
	Settings struct {
		Debug bool
		SkipDownloadSameSize bool
	}
}


func defaultConfig() Config {
	cfg := Config{}
	cfg.Settings.Debug = false
	cfg.Settings.SkipDownloadSameSize = false
	cfg.Destination.Arch = []string{"amd64"}
	return cfg
}


func validateConfig(cfg *Config) error {
	if len(cfg.Server.Source) == 0 {
		return fmt.Errorf("%s: [server].source, %s", _t("field") , _t("it i m a was n sp"))
	}
	if len(cfg.Destination.Path) == 0 {
		return fmt.Errorf("%s: [destination].path, %s", _t("field") , _t("it i m a was n sp"))
	}
	return nil
}