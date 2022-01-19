package portto

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

type Indexer struct {
	endpoint  string
	ethClient *Client
	repo      Repository

	jobs          chan uint64
	errors        chan error
	currentLatest uint64

	wg *sync.WaitGroup
}

const (
	ConfirmationNeeded = 10

	SecondPerBlock = 3

	// IndexLimit limits range to index when no block record in db, to avoid fetch whole chain
	IndexLimit = 100

	// Interval represent add new block to fetch queue (in second)
	Interval = 10

	MaxWorker = 3
)

func NewIndexer(endpoint string, repo Repository) (*Indexer, error) {
	c, err := NewClient(endpoint)
	if err != nil {
		return nil, err
	}
	return &Indexer{
		endpoint:  endpoint,
		ethClient: c,
		repo:      repo,
		//todo: should we use buffered channel here?
		jobs:   make(chan uint64, 1),
		errors: make(chan error),
		wg:     &sync.WaitGroup{},
	}, nil
}

func (idx *Indexer) Run(ctx context.Context) {
	idx.wg.Add(MaxWorker)
	for i := 0; i < MaxWorker; i++ {
		go idx.FastWorker(ctx, i, idx.endpoint)
	}

	idx.wg.Add(1)
	go idx.ConfirmWorker(ctx)

	for {
		select {
		case <-time.Tick(Interval * time.Second):
			idx.updateLatestNumber(ctx)
			idx.makeRoutineJobs(ctx)
		case err := <-idx.errors:
			log.Printf("error received : %v", err)
		case <-ctx.Done():
			return
		}
	}
}

func (idx *Indexer) updateLatestNumber(ctx context.Context) {
	latest, err := idx.ethClient.GetBlockNumber(ctx)
	if err != nil {
		idx.errors <- fmt.Errorf("failed to get latest number on chain, %v", err)
		return
	}
	idx.currentLatest = latest
}

func (idx *Indexer) makeRoutineJobs(ctx context.Context) {
	if len(idx.jobs) > 0 {
		log.Printf("[makeRoutineJobs] still got job to do, no new job(s) added")
		return
	}

	repoLatest, err := idx.repo.GetLatestNumber()
	if err != nil {
		idx.errors <- fmt.Errorf("failed to get latest number in DB, %v", err)
		return
	}

	// for dev purpose, limit index range when repo empty
	var from uint64
	if repoLatest == 0 {
		from = idx.currentLatest - IndexLimit
	} else {
		from = repoLatest + 1
	}

	log.Printf("adding new jobs to queue : from %d to %d\n", from, idx.currentLatest)
	for i := from; i <= idx.currentLatest; i++ {
		select {
		case <-ctx.Done():
			close(idx.jobs)
			log.Println("[makeRoutineJobs] stop add new job to queue, job channel closed")
			return
		default:
			idx.addJob(i)
		}
	}
}

func (idx *Indexer) addJob(n uint64) {
	go func() {
		idx.jobs <- n
	}()
}

func (idx *Indexer) FastWorker(ctx context.Context, id int, endpoint string) {
	defer idx.wg.Done()
	client, err := NewClient(endpoint)
	if err != nil {
		idx.errors <- fmt.Errorf("[FastWorker] failed to create Client, %v", err)
		return
	}

	for {
		select {
		case number, ok := <-idx.jobs:
			if !ok {
				log.Printf("[FastWorker] jobs channel closed, stop worker %d", id)
				return
			}

			err := idx.fetchAndStoreBlock(ctx, client, number)
			if err != nil {
				idx.errors <- fmt.Errorf("[FastWorker] failed to fetch block and store, %v", err)
			}
		case <-ctx.Done():
			log.Printf("[FastWorker] receive cancel singal, stop FastWorker %d", id)
			return
		}
	}
}

func (idx *Indexer) fetchAndStoreBlock(ctx context.Context, client *Client, number uint64) error {
	blockRaw, err := client.GetBlockByNumber(context.TODO(), number)
	if err != nil {
		return fmt.Errorf("[FastWorker] failed to get block, %v", err)
	}

	hashes := make([]string, len(blockRaw.Transactions()))
	for i, transaction := range blockRaw.Transactions() {
		hashes[i] = transaction.Hash().String()
	}
	blockModel := &Block{
		Number:       blockRaw.NumberU64(),
		Hash:         blockRaw.Hash().String(),
		Time:         blockRaw.Time(),
		ParentHash:   blockRaw.ParentHash().String(),
		Transactions: hashes,
	}

	_ = idx.repo.CreateBlock(blockModel)

	if err != nil {
		log.Printf("[FastWorker] create block error, %v", err)
		return err
	}

	return nil
}

func (idx *Indexer) ConfirmWorker(ctx context.Context) {
	defer idx.wg.Done()
	client, err := NewClient(idx.endpoint)
	if err != nil {
		idx.errors <- fmt.Errorf("[ConfirmWorker] failed to create Client in ConfirmWorker, %v", err)
		return
	}
	_ = client

	for {
		select {
		case <-time.Tick(ConfirmationNeeded * SecondPerBlock * time.Second):
			blocks, err := idx.repo.GetUnconfirmedBlocks()
			if err != nil {
				idx.errors <- fmt.Errorf("failed to get unconfirmed blocks ConfirmWorker, %v", err)
				return
			}

			if len(blocks) < 2 {
				log.Printf("[ConfirmWorker] no block to confirm\n")
				continue
			}

			to := int(idx.currentLatest - ConfirmationNeeded)
			log.Printf("[ConfirmWorker] checking block from %d to %d\n", int(blocks[0].Number), to)

			var validatedBlocks []*Block
			for i := 0; i < len(blocks)-1; i++ {
				if blocks[i].Number >= uint64(to) {
					break
				}

				if blocks[i].Hash != blocks[i+1].ParentHash {
					log.Printf("[ConfirmWorker] todo: update rest blocks from start from i+1\n")
					break
				}

				validatedBlocks = append(validatedBlocks, blocks[i])
			}
			log.Printf("[ConfirmWorker] %d blocks validated, update repo\n", len(validatedBlocks))

			err = idx.repo.ConfirmBlocks(validatedBlocks)
			if err != nil {
				idx.errors <- fmt.Errorf("[ConfirmWorker] failed to confirm blocks, %v", err)
				return
			}

		case <-ctx.Done():
			log.Printf("receive cancel singal, stop ConfirmWorker")
			return
		}
	}
}

func (idx *Indexer) StopWait() {
	log.Println("waiting for everything stop...")
	idx.wg.Wait()
}

// APIs

func (idx *Indexer) GetNewBlocks(limit int) ([]*Block, error) {
	blocks, err := idx.repo.GetNewBlocks(limit)
	for i := range blocks {
		blocks[i].Transactions = nil
	}

	return blocks, err
}

func (idx *Indexer) GetBlock(number uint64) (*Block, error) {
	block, err := idx.repo.FindBlock(number)
	if err != nil {
		return nil, fmt.Errorf("failed to get block from repo, %v", err)
	}

	return block, nil
}

func (idx *Indexer) GetTransaction(hash string) (*Transaction, error) {
	tx, err := idx.repo.FindTransaction(hash)
	if err != nil {
		return nil, fmt.Errorf("failed to FindTransaction, %v", err)
	}

	if tx == nil {
		tx, err := idx.ethClient.GetTransactionByHash(context.TODO(), hash)
		if err != nil {
			return nil, fmt.Errorf("failed to get transaction from chain, %v", err)
		}

		txReceipt, err := idx.ethClient.GetTransactionReceipt(context.TODO(), hash)
		if err != nil {
			return nil, fmt.Errorf("failed to get transaction receipt from chain, %v", err)
		}

		logs := make([]Log, len(txReceipt.Logs))
		for i, l := range txReceipt.Logs {
			logs[i] = Log{
				Index: uint64(l.Index),
				Data:  common.BytesToHash(l.Data).String(),
			}
		}

		t := &Transaction{
			Hash:      hash,
			From:      tx.From,
			To:        tx.To,
			Nonce:     tx.Nonce,
			Data:      tx.Data,
			Value:     tx.Value,
			Logs:      logs,
			BlockHash: txReceipt.BlockHash.String(),
		}

		err = idx.repo.CreateTransaction(t)
		if err != nil {
			return nil, fmt.Errorf("failed to store transaction to repo, %v", err)
		}

		return t, nil
	}

	return tx, nil
}
