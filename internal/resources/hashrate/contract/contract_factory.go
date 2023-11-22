package contract

import (
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/interfaces"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/lib"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/repositories/contracts"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/resources"
	hashrateContract "gitlab.com/TitanInd/proxy/proxy-router-v3/internal/resources/hashrate"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/resources/hashrate/allocator"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/resources/hashrate/hashrate"
)

type ContractFactory struct {
	// config
	privateKey               string // private key of the user
	cycleDuration            time.Duration
	shareTimeout             time.Duration
	hrErrorThreshold         float64
	hashrateCounterNameBuyer string
	validatorFlatness        time.Duration

	// state
	address common.Address // derived from private key

	// deps
	store           *contracts.HashrateEthereum
	allocator       *allocator.Allocator
	globalHashrate  *hashrate.GlobalHashrate
	hashrateFactory func() *hashrate.Hashrate
	logFactory      func(contractID string) (interfaces.ILogger, error)
}

func NewContractFactory(
	allocator *allocator.Allocator,
	hashrateFactory func() *hashrate.Hashrate,
	globalHashrate *hashrate.GlobalHashrate,
	store *contracts.HashrateEthereum,
	logFactory func(contractID string) (interfaces.ILogger, error),

	privateKey string,
	cycleDuration time.Duration,
	shareTimeout time.Duration,
	hrErrorThreshold float64,
	hashrateCounterNameBuyer string,
	validatorFlatness time.Duration,
) (*ContractFactory, error) {
	address, err := lib.PrivKeyStringToAddr(privateKey)
	if err != nil {
		return nil, err
	}

	return &ContractFactory{
		allocator:       allocator,
		hashrateFactory: hashrateFactory,
		globalHashrate:  globalHashrate,
		store:           store,
		logFactory:      logFactory,

		address: address,

		privateKey:               privateKey,
		cycleDuration:            cycleDuration,
		shareTimeout:             shareTimeout,
		hrErrorThreshold:         hrErrorThreshold,
		hashrateCounterNameBuyer: hashrateCounterNameBuyer,
		validatorFlatness:        validatorFlatness,
	}, nil
}

func (c *ContractFactory) CreateContract(contractData *hashrateContract.EncryptedTerms) (resources.Contract, error) {
	log, err := c.logFactory(lib.AddrShort(contractData.ID()))
	if err != nil {
		return nil, err
	}

	defer func() { _ = log.Sync() }()

	logNamed := log.Named(lib.AddrShort(contractData.ID()))

	if contractData.Seller() == c.address.String() {
		terms, err := contractData.Decrypt(c.privateKey)
		if err != nil {
			return nil, err
		}
		watcher := NewContractWatcherSellerV2(terms, c.cycleDuration, c.hashrateFactory, c.allocator, logNamed)
		return NewControllerSeller(watcher, c.store, c.privateKey), nil
	}
	if contractData.Buyer() == c.address.String() {
		watcher := NewContractWatcherBuyer(
			contractData,
			c.hashrateFactory,
			c.allocator,
			c.globalHashrate,
			logNamed,

			c.cycleDuration,
			c.shareTimeout,
			c.hrErrorThreshold,
			c.hashrateCounterNameBuyer,
			c.validatorFlatness,
		)
		return NewControllerBuyer(watcher, c.store, c.privateKey), nil
	}
	return nil, fmt.Errorf("invalid terms %+v", contractData)
}

func (c *ContractFactory) GetType() resources.ResourceType {
	return ResourceTypeHashrate
}
