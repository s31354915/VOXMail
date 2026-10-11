package speech

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/voxmail/voxmail/internal/netguard"
)

// AdministratorVoiceRequest is accepted only by the web administrator endpoint.
// Named voices resolve through the Piper repository; an explicit HTTPS model
// source permits custom voices without changing the bundled trusted catalog.
type AdministratorVoiceRequest struct {
	Voice        string `json:"voice"`
	ModelURL     string `json:"model_url"`
	ConfigURL    string `json:"config_url"`
	ModelSHA256  string `json:"model_sha256"`
	ConfigSHA256 string `json:"config_sha256"`
}

func validateVoiceURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("voice source must be an HTTPS URL without credentials or a fragment")
	}
	if u.Port() != "" {
		if port, err := strconv.Atoi(u.Port()); err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("voice source port is invalid")
		}
	}
	return u, nil
}

// ResolveAdministratorVoice performs no network work. Actual download remains
// asynchronous, cancelable and separately restricted to the administrator path.
func ResolveAdministratorVoice(request AdministratorVoiceRequest) (VoiceSpec, error) {
	name := strings.TrimSpace(request.Voice)
	modelURL := strings.TrimSpace(request.ModelURL)
	if strings.Contains(name, "://") {
		if modelURL != "" {
			return VoiceSpec{}, fmt.Errorf("supply a voice name or a model URL, not two model URLs")
		}
		modelURL = name
		name = ""
	}
	if modelURL == "" {
		if trusted, ok := FindTrustedVoice(name); ok && request.ConfigURL == "" && request.ModelSHA256 == "" && request.ConfigSHA256 == "" {
			return trusted, nil
		}
		var ok bool
		name, ok = normalizeVoiceName(name)
		if !ok {
			return VoiceSpec{}, fmt.Errorf("voice name is invalid")
		}
		locale, rest, ok := strings.Cut(name, "-")
		last := strings.LastIndex(rest, "-")
		if !ok || last <= 0 || last == len(rest)-1 || !voicePathComponent(locale) || !voicePathComponent(rest[:last]) || !voicePathComponent(rest[last+1:]) {
			return VoiceSpec{}, fmt.Errorf("enter a Piper voice name such as en_US-lessac-medium, or an HTTPS .onnx URL")
		}
		language, _, _ := strings.Cut(locale, "_")
		modelURL = "https://huggingface.co/rhasspy/piper-voices/resolve/main/" + language + "/" + locale + "/" + rest[:last] + "/" + rest[last+1:] + "/" + name + ".onnx?download=true"
	}
	model, err := validateVoiceURL(modelURL)
	if err != nil {
		return VoiceSpec{}, err
	}
	if !strings.HasSuffix(model.Path, ".onnx") {
		return VoiceSpec{}, fmt.Errorf("voice model URL must name an .onnx file")
	}
	if name == "" {
		name = strings.TrimSuffix(path.Base(model.Path), ".onnx")
	}
	name, ok := normalizeVoiceName(name)
	if !ok || len(name) > 128 {
		return VoiceSpec{}, fmt.Errorf("voice name is invalid")
	}
	configURL := strings.TrimSpace(request.ConfigURL)
	if configURL == "" {
		config := *model
		config.Path += ".json"
		config.RawPath = ""
		configURL = config.String()
	}
	config, err := validateVoiceURL(configURL)
	if err != nil {
		return VoiceSpec{}, err
	}
	modelHash := strings.ToLower(strings.TrimSpace(request.ModelSHA256))
	configHash := strings.ToLower(strings.TrimSpace(request.ConfigSHA256))
	if modelHash != "" && !validSHA256(modelHash) || configHash != "" && !validSHA256(configHash) {
		return VoiceSpec{}, fmt.Errorf("voice checksums must contain 64 hexadecimal SHA-256 digits")
	}
	return VoiceSpec{Voice: name, ModelURL: model.String(), ConfigURL: config.String(), ModelSHA256: modelHash, ConfigSHA256: configHash}, nil
}

func voicePathComponent(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r != '_' && r != '-' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// AdministratorVoiceInstaller does not change InstallVoice's fixed-catalog
// policy. Client is a deployment-owned dependency for controlled fixtures;
// browser input cannot select a transport or bypass outbound address checks.
type AdministratorVoiceInstaller struct {
	Client *http.Client
	// The application uses actual Piper synthesis against the staged pair.
	// Failed validation leaves the previous active pointer untouched.
	Validate func(context.Context, string) error
}

func administratorDownloadClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Dial the validated endpoint, never an environment proxy.
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.DialContext = func(ctx context.Context, _ string, address string) (net.Conn, error) {
		host, portText, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return nil, err
		}
		return netguard.DialContext(ctx, host, port, "", false)
	}
	return &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("voice download exceeded redirect limit")
		}
		_, err := validateVoiceURL(req.URL.String())
		return err
	}}
}

func (i AdministratorVoiceInstaller) Install(ctx context.Context, voiceDir string, voice VoiceSpec) error {
	// Revalidate specs provided by trusted Go callers as well as web decoding.
	spec, err := ResolveAdministratorVoice(AdministratorVoiceRequest{Voice: voice.Voice, ModelURL: voice.ModelURL, ConfigURL: voice.ConfigURL, ModelSHA256: voice.ModelSHA256, ConfigSHA256: voice.ConfigSHA256})
	if err != nil {
		return err
	}
	operation, err := AcquireVoiceOperation(ctx, voiceDir, spec.Voice)
	if err != nil {
		return err
	}
	defer operation.Close()
	client := i.Client
	if client == nil {
		client = administratorDownloadClient()
		defer client.CloseIdleConnections()
	}
	return installVoiceWithDownload(ctx, voiceDir, spec, func(ctx context.Context, source, destination, checksum string) error {
		limit := int64(maxModelDownload)
		if strings.HasSuffix(destination, ".onnx.json") {
			limit = 16 << 20
		}
		return downloadVerifiedLimit(ctx, client, source, destination, checksum, limit)
	}, i.Validate)
}
