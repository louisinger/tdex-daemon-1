package operatorservice

import (
	"context"
	"encoding/hex"
	"github.com/tdex-network/tdex-daemon/pkg/crawler"

	"github.com/tdex-network/tdex-daemon/internal/domain/vault"
	pb "github.com/tdex-network/tdex-protobuf/generated/go/operator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DepositFeeAccount returns a new address for the fee account
func (s *Service) DepositFeeAccount(ctx context.Context, req *pb.DepositFeeAccountRequest) (reply *pb.DepositFeeAccountReply, err error) {
	if err = s.vaultRepository.UpdateVault(ctx, nil, "", func(v *vault.Vault) (*vault.Vault, error) {
		addr, _, blindingKey, err := v.DeriveNextExternalAddressForAccount(vault.FeeAccount)
		if err != nil {
			return nil, err
		}

		reply = &pb.DepositFeeAccountReply{
			Address:  addr,
			Blinding: hex.EncodeToString(blindingKey),
		}

		s.crawlerSvc.AddObservable(&crawler.AddressObservable{
			AccountIndex: vault.FeeAccount,
			Address:      addr,
			BlindingKey:  blindingKey,
		})

		return v, nil
	}); err != nil {
		err = status.Error(codes.Internal, err.Error())
		return
	}
	return
}