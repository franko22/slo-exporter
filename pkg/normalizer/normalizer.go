package normalizer

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/viper"
	"gitlab.seznam.net/sklik-devops/slo-exporter/pkg/event"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/asaskevich/govalidator"
	"github.com/sirupsen/logrus"
)

const (
	eventKeyFieldSeparator = ":"
	numberPlaceholder      = "0"
	ipPlaceholder          = ":ip"
	hashPlaceholder        = ":hash"
	uuidPlaceholder        = ":uuid"
	imagePlaceholder       = ":image"
	fontPlaceholder        = ":font"
	pathItemsSeparator     = "/"
	component              = "normalizer"
)

var (
	log                 *logrus.Entry
	imageExtensionRegex = regexp.MustCompile(`(?i)\.(?:png|jpg|jpeg|svg|tif|tiff|gif|ico)$`)
	fontExtensionRegex  = regexp.MustCompile(`(?i)\.(?:ttf|woff)$`)
)

func init() {
	log = logrus.WithField("component", component)
}

type replacer struct {
	regexpCompiled *regexp.Regexp
	Regexp         string
	Replacement    string
}

func (n *replacer) process(path string) string {
	if n.regexpCompiled == nil {
		n.regexpCompiled = regexp.MustCompile(n.Regexp)
	}
	if n.regexpCompiled.MatchString(path) {
		return n.Replacement
	}
	return path
}

func NewFromViper(viperConfig *viper.Viper) (*requestNormalizer, error) {
	config := &requestNormalizerConfig{}
	if err := viperConfig.UnmarshalExact(config); err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}
	return NewFromConfig(config)
}

type requestNormalizerConfig struct {
	GetParamWithEventIdentifier string
	ReplaceRules                []replacer
	SanitizeHashes              bool
	SanitizeNumbers             bool
	SanitizeUids                bool
	SanitizeIps                 bool
	SanitizeImages              bool
	SanitizeFonts               bool
}

// New returns requestNormalizer which allows to add Key to RequestEvent
func NewFromConfig(config *requestNormalizerConfig) (*requestNormalizer, error) {
	normalizer := requestNormalizer{
		getParamWithEventIdentifier: config.GetParamWithEventIdentifier,
		replaceRules:                config.ReplaceRules,
		sanitizeHashes:              config.SanitizeHashes,
		sanitizeNumbers:             config.SanitizeNumbers,
		sanitizeUids:                config.SanitizeUids,
		sanitizeIps:                 config.SanitizeIps,
		sanitizeImages:              config.SanitizeImages,
		sanitizeFonts:               config.SanitizeFonts,
	}
	if err := normalizer.precompileRegexps(); err != nil {
		return nil, err
	}
	return &normalizer, nil
}

type requestNormalizer struct {
	getParamWithEventIdentifier string
	replaceRules                []replacer
	sanitizeHashes              bool
	sanitizeNumbers             bool
	sanitizeUids                bool
	sanitizeIps                 bool
	sanitizeImages              bool
	sanitizeFonts               bool
	observer                    prometheus.Observer
}

func (rn *requestNormalizer) SetPrometheusObserver(observer prometheus.Observer) {
	rn.observer = observer
}

func (rn *requestNormalizer) observeDuration(start time.Time) {
	if rn.observer != nil {
		rn.observer.Observe(time.Since(start).Seconds())
	}
}

func (rn *requestNormalizer) precompileRegexps() error {
	for i, rep := range rn.replaceRules {
		compiled, err := regexp.Compile(rep.Regexp)
		if err != nil {
			return fmt.Errorf("failed to compile Regexp %s: %w", rep.Regexp, err)
		}
		rn.replaceRules[i].regexpCompiled = compiled
	}
	return nil
}

func (rn *requestNormalizer) normalizePath(rawPath string) string {
	if rawPath == "" {
		return "/"
	}
	for _, rule := range rn.replaceRules {
		rawPath = rule.process(rawPath)
	}
	pathItems := strings.Split(path.Clean(rawPath), pathItemsSeparator)
	itemsCount := len(pathItems)
	for i, item := range pathItems {
		if item == "" {
			continue
		}

		if rn.sanitizeHashes && (govalidator.IsMD5(item) || govalidator.IsSHA1(item) || govalidator.IsSHA256(item)) {
			pathItems[i] = hashPlaceholder
			continue
		}
		if rn.sanitizeNumbers && (govalidator.IsNumeric(item) || govalidator.IsHexadecimal(item)) {
			pathItems[i] = numberPlaceholder
			continue
		}

		if rn.sanitizeUids && (govalidator.IsUUID(item) || govalidator.IsUUIDv4(item)) {
			pathItems[i] = uuidPlaceholder
			continue
		}

		if rn.sanitizeIps && govalidator.IsIP(item) {
			pathItems[i] = ipPlaceholder
			continue
		}

		// replace all numbers with zero in the last part of the rawPath
		if i+1 == itemsCount {
			if rn.sanitizeImages && imageExtensionRegex.MatchString(item) {
				pathItems[i] = imagePlaceholder
				continue
			}
			if rn.sanitizeFonts && fontExtensionRegex.MatchString(item) {
				pathItems[i] = fontPlaceholder
				continue
			}
			continue
		}
	}
	return strings.Join(pathItems, pathItemsSeparator)
}

func (rn *requestNormalizer) getNormalizedEventKey(event *event.HttpRequest) string {
	var eventIdentifiers = []string{event.Method}
	eventIdentifiers = append(eventIdentifiers, rn.normalizePath(event.URL.Path))
	if rn.getParamWithEventIdentifier != "" {
		// Append all values of configured get parameter
		operationNames, ok := event.URL.Query()[rn.getParamWithEventIdentifier]
		if ok {
			for _, operation := range operationNames {
				eventIdentifiers = append(eventIdentifiers, operation)
			}
		}
	}
	return strings.Join(eventIdentifiers, eventKeyFieldSeparator)
}

// Run event replacer receiving events and filling their Key if not already filled.
func (rn *requestNormalizer) Run(inputEventsChan <-chan *event.HttpRequest, outputEventsChan chan<- *event.HttpRequest) {
	go func() {
		defer close(outputEventsChan)
		for newEvent := range inputEventsChan {
			start := time.Now()
			if newEvent.EventKey != "" {
				log.Debugf("skipping newEvent normalization, already has Key: %s", newEvent.EventKey)
			} else {
				newEvent.EventKey = rn.getNormalizedEventKey(newEvent)
				log.Debugf("processed newEvent with Key: %s", newEvent.EventKey)
			}
			outputEventsChan <- newEvent
			rn.observeDuration(start)
		}
		log.Info("input channel closed, finishing")
	}()
}
