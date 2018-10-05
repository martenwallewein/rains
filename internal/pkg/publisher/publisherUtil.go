package publisher

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"time"

	log "github.com/inconshreveable/log15"

	"github.com/netsec-ethz/rains/internal/pkg/algorithmTypes"
	"github.com/netsec-ethz/rains/internal/pkg/connection"
	"github.com/netsec-ethz/rains/internal/pkg/keys"
	"github.com/netsec-ethz/rains/internal/pkg/section"
	"github.com/netsec-ethz/rains/internal/pkg/siglib"
	"github.com/netsec-ethz/rains/internal/pkg/zonefile"
	"golang.org/x/crypto/ed25519"
)

//Config lists configurations for publishing zone information, see zonepub flag description for
//detail.
type Config struct {
	ZonefilePath    string
	AuthServers     []connection.Info
	PrivateKeyPath  string
	ShardingConf    ShardingConfig
	PShardingConf   PShardingConfig
	MetaDataConf    MetaDataConfig
	ConsistencyConf ConsistencyConfig
	DoSigning       bool
	MaxZoneSize     int
	OutputPath      string
	DoPublish       bool
}

//ShardingConfig contains configuration options on how to split a zone into shards.
type ShardingConfig struct {
	DoSharding            bool
	KeepExistingShards    bool
	NofAssertionsPerShard int
	MaxShardSize          int
}

//PShardingConfig contains configuration options on how to split a zone into probabilistic shards.
type PShardingConfig struct {
	DoPsharding            bool
	KeepExistingPshards    bool
	NofAssertionsPerPshard int
	BloomFilterConf        BloomFilterConfig
}

//BloomFilterConfig specifies the bloom filter's meta data
type BloomFilterConfig struct {
	Hashfamily       []algorithmTypes.Hash
	NofHashFunctions int
	BFOpMode         section.ModeOfOperationType
	BloomFilterSize  int
}

//MetaDataConfig determines how the signature meta data is generated and to which section(s) it is
//added.
type MetaDataConfig struct {
	AddSignatureMetaData       bool
	AddSigMetaDataToAssertions bool
	AddSigMetaDataToShards     bool
	AddSigMetaDataToPshards    bool
	SignatureAlgorithm         algorithmTypes.Signature
	KeyPhase                   int
	SigValidSince              time.Duration
	SigValidUntil              time.Duration
	SigSigningInterval         time.Duration
}

//ConsistencyConfig determines which consistency checks are performed prior to signing.
type ConsistencyConfig struct {
	DoConsistencyCheck bool
	SortShards         bool
	SigNotExpired      bool
	CheckStringFields  bool
}

//LoadConfig loads configuration information from configPath
func LoadConfig(configPath string) (Config, error) {
	var config Config
	file, err := ioutil.ReadFile(configPath)
	if err != nil {
		log.Error("Could not open config file...", "path", configPath, "error", err)
		return Config{}, err
	}
	if err = json.Unmarshal(file, &config); err != nil {
		log.Error("Could not unmarshal json format of config", "error", err)
		return Config{}, err
	}
	config.MetaDataConf.SigValidSince *= time.Second
	config.MetaDataConf.SigValidUntil *= time.Second
	config.MetaDataConf.SigSigningInterval *= time.Second
	return config, nil
}

//loadZonefile loads the zonefile from disk.
func loadZonefile(path string, parser zonefile.ZoneFileParser) (*section.Zone, error) {
	file, err := ioutil.ReadFile(path)
	if err != nil {
		log.Error("Was not able to read zone file", "path", path)
		return nil, err
	}
	//FIXME CFE replace with call to yacc generated zonefile parser.
	zone, err := parser.DecodeZone(file)
	if err != nil {
		log.Error("Was not able to parse zone file.", "error", err)
		return nil, err
	}
	return zone, nil
}

//loadPrivateKeys reads private keys from the path provided in the config and returns a map from
//PublicKeyID to the corresponding private key data.
func loadPrivateKeys(path string) (map[keys.PublicKeyID]interface{}, error) {
	var privateKeys []keys.PrivateKey
	file, err := ioutil.ReadFile(path)
	if err != nil {
		log.Error("Could not open config file...", "path", path, "error", err)
		return nil, err
	}
	if err = json.Unmarshal(file, &privateKeys); err != nil {
		log.Error("Could not unmarshal json format of private keys", "error", err)
		return nil, err
	}
	output := make(map[keys.PublicKeyID]interface{})
	for _, keyData := range privateKeys {
		keyString := keyData.Key.(string)
		privateKey := make([]byte, hex.DecodedLen(len([]byte(keyString))))
		privateKey, err := hex.DecodeString(keyString)
		if err != nil {
			log.Error("Was not able to decode privateKey", "error", err)
			return nil, err
		}
		if len(privateKey) != ed25519.PrivateKeySize {
			log.Error("Private key length is incorrect", "expected", ed25519.PrivateKeySize,
				"actual", len(privateKey))
			return nil, errors.New("incorrect private key length")
		}
		output[keyData.PublicKeyID] = privateKey
	}
	return output, nil
}

//signZone signs the zone and all contained shards and assertions with the zone's private key. It
//removes the subjectZone and context of the contained assertions and shards after the signatures
//have been added. It returns an error if it was unable to sign the zone or any of the contained
//shards and assertions.
func signZone(zone *section.Zone, path string) error {
	if zone == nil {
		return errors.New("zone is nil")
	}
	keys, err := loadPrivateKeys(path)
	if err != nil {
		return errors.New("Was not able to load private keys")
	}
	for _, sig := range zone.Signatures {
		if ok := siglib.SignSectionUnsafe(zone, keys[sig.PublicKeyID], sig); !ok {
			log.Error("Was not able to sign and add the signature", "zone", zone, "signature", sig)
			return errors.New("Was not able to sign and add the signature")
		}
	}
	for _, sec := range zone.Content {
		switch sec := sec.(type) {
		case *section.Assertion:
			if err := signAssertion(sec, keys); err != nil {
				return err
			}
			sec.Context = ""
			sec.SubjectZone = ""
		case *section.Shard:
			if err := signShard(sec, keys); err != nil {
				return err
			}
			sec.Context = ""
			sec.SubjectZone = ""
		default:
			return fmt.Errorf("Zone contained unexpected type expected *Shard or *Assertion actual=%T", sec)
		}
	}
	return nil
}

//signShard signs the shard and all contained assertions with the zone's private key. It removes the
//subjectZone and context of the contained assertions after the signatures have been added. It
//returns an error if it was unable to sign the shard or any of the assertions.
func signShard(s *section.Shard, keys map[keys.PublicKeyID]interface{}) error {
	if s == nil {
		return errors.New("shard is nil")
	}
	for _, sig := range s.Signatures {
		if ok := siglib.SignSectionUnsafe(s, keys[sig.PublicKeyID], sig); !ok {
			log.Error("Was not able to sign and add the signature", "shard", s, "signature", sig)
			return errors.New("Was not able to sign and add the signature")
		}
	}
	for _, a := range s.Content {
		if err := signAssertion(a, keys); err != nil {
			return err
		}
		a.Context = ""
		a.SubjectZone = ""
	}
	return nil
}

//signAssertion computes the signature data for all contained signatures.
//It returns an error if it was unable to create all signatures on the assertion.
func signAssertion(a *section.Assertion, keys map[keys.PublicKeyID]interface{}) error {
	if a == nil {
		return errors.New("assertion is nil")
	}
	for _, sig := range a.Signatures {
		if ok := siglib.SignSectionUnsafe(a, keys[sig.PublicKeyID], sig); !ok {
			log.Error("Was not able to sign and add the signature", "assertion", a, "signature", sig)
			return errors.New("Was not able to sign and add the signature")
		}
	}
	return nil
}
