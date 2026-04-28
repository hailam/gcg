package config

type Config struct {
	App  App  `yaml:"app"`
	LLM  LLM  `yaml:"llm"`
	Diff Diff `yaml:"diff"`
}

type App struct {
	Name    string `yaml:"name"`
	Env     string `yaml:"env"`
	Version string `yaml:"version"`
}

type LLM struct {
	Host  string `yaml:"host"`
	Model string `yaml:"model"`
}

type Diff struct {
	MaxBytes int `yaml:"max_bytes"`
}
