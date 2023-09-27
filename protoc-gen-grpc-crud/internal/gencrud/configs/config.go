package configs

var Confs Config

type Config struct {
	AclImport            string `yaml:"AclImport"`
	UtilImport           string `yaml:"UtilImport"`
	AuthenticationImport string `yaml:"AuthenticationImport"`
}
