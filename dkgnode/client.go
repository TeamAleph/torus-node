package dkgnode

/* All useful imports */
import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	"time"

	"github.com/YZhenY/DKGNode/pvss"
	"github.com/decred/dcrd/dcrec/secp256k1"
	"github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	jsonrpcclient "github.com/ybbus/jsonrpc"
)

// TODO: pass in as config
const NumberOfShares = 1300 // potentially 1.35 mm, assuming 7.5k uniques a day
const BftURI = "http://localhost:7053/jrpc"

type NodeReference struct {
	Address    *common.Address
	JSONClient jsonrpcclient.RPCClient
	Index      *big.Int
	PublicKey  *ecdsa.PublicKey
}

type Person struct {
	Name string `json:"name"`
}

type Message struct {
	Message string `json:"message"`
}

type SecretStore struct {
	Secret   *big.Int
	Assigned bool
}

type SecretAssignment struct {
	Secret     *big.Int
	ShareIndex int
	Share      *big.Int
}

type SigncryptedMessage struct {
	FromAddress string `json:"fromaddress"`
	FromPubKeyX string `json:"frompubkeyx"`
	FromPubKeyY string `json:"frompubkeyy"`
	Ciphertext  string `json:"ciphertext"`
	RX          string `json:"rx"`
	RY          string `json:"ry"`
	Signature   string `json:"signature"`
	ShareIndex  int    `json:"shareindex"`
}

type PubPolyProof struct {
	EcdsaSignature   ECDSASignature
	PointsBytesArray []byte
}

func setUpClient(nodeListStrings []string) {
	// nodeListStruct make(NodeReference[], 0)
	// for index, element := range nodeListStrings {
	time.Sleep(1000 * time.Millisecond)
	for {
		rpcClient := jsonrpcclient.NewClient(nodeListStrings[0])

		response, err := rpcClient.Call("Main.Echo", &Person{"John"})
		if err != nil {
			fmt.Println("couldnt connect")
		}

		fmt.Println("response: ", response)
		fmt.Println(time.Now().UTC())
		time.Sleep(1000 * time.Millisecond)
	}
	// }
}

func keyGenerationPhase(suite *Suite) (string, error) {
	time.Sleep(1000 * time.Millisecond) // TODO: wait for servers to spin up
	bftRPC := NewBftRPC(BftURI)
	nodeList := make([]*NodeReference, suite.Config.NumberOfNodes)
	siMapping := make(map[int]pvss.PrimaryShare)
	for {
		// Fetch Node List from contract address
		ethList, err := suite.EthSuite.NodeListInstance.ViewNodeList(nil)
		if err != nil {
			fmt.Println(err)
		}
		if len(ethList) > 0 {
			fmt.Println("Connecting to other nodes ------------------")
			// Build count of nodes connected to
			triggerSecretSharing := 0
			for i := range ethList {
				// fmt.Println(ethList[i].Hex())

				// Check if node is online
				temp, err := connectToJSONRPCNode(suite, ethList[i])
				if err != nil {
					fmt.Println(err)
				}
				// fmt.Println("ERROR HERE", int(temp.Index.Int64()))
				if temp != nil {
					if nodeList[int(temp.Index.Int64())-1] == nil {
						nodeList[int(temp.Index.Int64())-1] = temp
					} else {
						triggerSecretSharing++
					}
				}
			}

			if triggerSecretSharing > suite.Config.NumberOfNodes {
				log.Fatal("There are more nodes in the eth node list than the required number of nodes... exiting...")
			}

			// if we have connected to all nodes
			if triggerSecretSharing == suite.Config.NumberOfNodes {
				fmt.Println("Required number of nodes reached")
				fmt.Println("Sending shares -----------")
				numberOfShares := NumberOfShares
				secretMapping := make(map[int]SecretStore)
				for shareIndex := 0; shareIndex < numberOfShares; shareIndex++ {
					nodes := make([]pvss.Point, suite.Config.NumberOfNodes)

					for i := 0; i < triggerSecretSharing; i++ {
						nodes[i] = *ecdsaPttoPt(nodeList[i].PublicKey)
					}

					// this is the secret zi generated by each node
					secret := pvss.RandomBigInt()
					// fmt.Println("Node "+suite.EthSuite.NodeAddress.Hex(), " Secret: ", secret.Text(16))

					// create shares and public polynomial commitment
					shares, pubpoly, err := pvss.CreateShares(nodes, *secret, suite.Config.Threshold, *suite.EthSuite.NodePrivateKey.D)
					if err != nil {
						fmt.Println(err)
					}

					// commit pubpoly by signing it and broadcasting it

					// sign hash of pubpoly by converting array of points to bytes array
					arrBytes := PointsArrayToBytesArray(pubpoly)
					ecdsaSignature := ECDSASign(arrBytes, suite.EthSuite.NodePrivateKey) // TODO: check if it matches on-chain implementation

					pubPolyProof := PubPolyProof{EcdsaSignature: ecdsaSignature, PointsBytesArray: arrBytes}

					jsonData, err := json.Marshal(pubPolyProof)
					if err != nil {
						fmt.Println("Error with marshalling signed pubpoly")
						fmt.Println(err)
						return "", err
					}

					// broadcast signed pubpoly
					id, err := bftRPC.Broadcast(jsonData)
					if err != nil {
						fmt.Println("Can't broadcast signed pubpoly")
						fmt.Println(err)
					}
					fmt.Println(id)

					// signcrypt data
					signcryptedData := make([]*pvss.SigncryptedOutput, len(nodes))
					for index, share := range *shares {
						// serializing id + primary share value into bytes before signcryption
						var data []byte
						data = append(data, share.Value.Bytes()...)
						data = append(data, big.NewInt(int64(id)).Bytes()...) // length of big.Int is 2 bytes
						signcryption, err := pvss.Signcrypt(nodes[index], data, *suite.EthSuite.NodePrivateKey.D)
						if err != nil {
							fmt.Println("Failed during signcryption")
						}
						signcryptedData[index] = &pvss.SigncryptedOutput{NodePubKey: nodes[index], NodeIndex: share.Index, SigncryptedShare: *signcryption}
					}

					errArr := sendSharesToNodes(*suite.EthSuite, signcryptedData, nodeList, shareIndex)
					if errArr != nil {
						fmt.Println("errors sending shares")
						fmt.Println(errArr)
					}
					secretMapping[shareIndex] = SecretStore{secret, false}
				}

				// Signcrypted shares are received by the other nodes and handled in server.go

				time.Sleep(8000 * time.Millisecond) // TODO: Check for communication termination from all other nodes
				// gather shares, decrypt and verify with pubpoly
				// - check if shares are here
				// Approach: for each shareIndex, we gather all shares shared by nodes for that share index
				// we retrieve the broadcasted signature via the broadcastID for each share and verify its correct
				// we then addmod all shares and get our actual final share
				for shareIndex := 0; shareIndex < numberOfShares; shareIndex++ {
					var unsigncryptedShares []*big.Int
					var broadcastIdArray []int
					var nodePubKeyArray []*ecdsa.PublicKey
					var nodeId []int
					for i := 0; i < suite.Config.NumberOfNodes; i++ { // TODO: inefficient, we are looping unnecessarily
						data, found := suite.CacheSuite.CacheInstance.Get(nodeList[i].Address.Hex() + "_MAPPING")
						if found {
							var shareMapping = data.(map[int]ShareLog)
							if val, ok := shareMapping[shareIndex]; ok {
								unsigncryptedShares = append(unsigncryptedShares, new(big.Int).SetBytes(val.UnsigncryptedShare))
								broadcastIdArray = append(broadcastIdArray, val.BroadcastId)
								nodePubKeyArray = append(nodePubKeyArray, nodeList[i].PublicKey)
								nodeId = append(nodeId, i+1)
							}
						} else {
							fmt.Println("Could not find mapping for node ", i)
							break
						}
					}
					// Retrieve previously broadcasted signed pubpoly data
					broadcastedDataArray := make([][]*pvss.Point, len(broadcastIdArray))
					for index, broadcastId := range broadcastIdArray {
						fmt.Println("BROADCASTID WAS: ", broadcastId)
						jsonData, _, err := bftRPC.Retrieve(broadcastId) // TODO: use a goroutine to run this concurrently
						if err != nil {
							fmt.Println("Could not retrieve broadcast")
							fmt.Println(err)
							continue
						}
						data := &PubPolyProof{}
						fmt.Println("jsonData was ", jsonData)
						if err := json.Unmarshal(jsonData, &data); err != nil {
							fmt.Println("Could not unmarshal json data")
							fmt.Println(err)
							fmt.Println(jsonData)
							continue
						}
						fmt.Println("jsonData was unmarshaled into ", data)
						hashedData := bytes32(ethCrypto.Keccak256(data.PointsBytesArray))
						if bytes.Compare(data.EcdsaSignature.Hash[:], hashedData[:]) != 0 {
							fmt.Println("Signed hash does not match retrieved hash")
							fmt.Println(data.EcdsaSignature.Hash[:])
							fmt.Println(hashedData[:])
							continue
						}
						if !ECDSAVerify(*nodePubKeyArray[index], data.EcdsaSignature) {
							fmt.Println("Signature does not verify")
							continue
						} else {
							fmt.Println("Signature of pubpoly verified")
						}
						broadcastedDataArray[index] = BytesArrayToPointsArray(data.PointsBytesArray)
					}

					// verify share against pubpoly
					s := secp256k1.S256()
					for index, pubpoly := range broadcastedDataArray {
						var sumX, sumY = big.NewInt(int64(0)), big.NewInt(int64(0))
						var myNodeReference *NodeReference
						for _, noderef := range nodeList {
							if noderef.Address.Hex() == suite.EthSuite.NodeAddress.Hex() {
								myNodeReference = noderef
							}
						}
						nodeI := myNodeReference.Index
						fmt.Println("nodeI ", nodeI)
						for ind, pt := range pubpoly {
							x_i := new(big.Int)
							x_i.Exp(nodeI, big.NewInt(int64(ind)), pvss.GeneratorOrder)
							tempX, tempY := s.ScalarMult(&pt.X, &pt.Y, x_i.Bytes())
							sumX, sumY = s.Add(sumX, sumY, tempX, tempY)
						}
						fmt.Println("SHOULD EQL PUB", sumX, sumY)
						subshare := unsigncryptedShares[index]
						tempX, tempY := s.ScalarBaseMult(subshare.Bytes())
						fmt.Println("SHOULD EQL REC", tempX, tempY)
						if sumX.Text(16) != tempX.Text(16) || sumY.Text(16) != tempY.Text(16) {
							fmt.Println("Could not verify share from node")
						} else {
							fmt.Println("Share verified")
						}
					}

					// form Si
					tempSi := new(big.Int)
					for i := range unsigncryptedShares {
						tempSi.Add(tempSi, unsigncryptedShares[i])
					}
					tempSi.Mod(tempSi, pvss.GeneratorOrder)
					var nodeIndex int
					for i := range unsigncryptedShares {
						if nodeList[i].Address.Hex() == suite.EthSuite.NodeAddress.Hex() {
							nodeIndex = int(nodeList[i].Index.Int64())
						}
					}
					si := pvss.PrimaryShare{Index: nodeIndex, Value: *tempSi}
					fmt.Println("STORED Si: ", shareIndex)
					siMapping[shareIndex] = si
				}
				suite.CacheSuite.CacheInstance.Set("Si_MAPPING", siMapping, -1)
				suite.CacheSuite.CacheInstance.Set("Secret_MAPPING", secretMapping, -1)
				//save cache
				cacheItems := suite.CacheSuite.CacheInstance.Items()
				cacheJSON, err := json.Marshal(cacheItems)
				if err != nil {
					fmt.Println(err)
				}
				err = ioutil.WriteFile("cache.json", cacheJSON, 0644)
				if err != nil {
					fmt.Println(err)
				}
				break
			}
		} else {
			fmt.Println("No nodes in list/could not get from eth")
		}
		time.Sleep(5000 * time.Millisecond)
	}
	return "Keygen complete.", nil
}

func sendSharesToNodes(ethSuite EthSuite, signcryptedOutput []*pvss.SigncryptedOutput, nodeList []*NodeReference, shareIndex int) *[]error {
	errorSlice := make([]error, len(signcryptedOutput))
	// fmt.Println("GIVEN SIGNCRYPTION")
	// fmt.Println(signcryptedOutput[0].SigncryptedShare.Ciphertext)
	for i := range signcryptedOutput {
		for j := range signcryptedOutput { // TODO: this is because we aren't sure about the ordering of nodeList/signcryptedOutput...
			if signcryptedOutput[i].NodePubKey.X.Cmp(nodeList[j].PublicKey.X) == 0 {
				_, err := nodeList[j].JSONClient.Call("KeyGeneration.ShareCollection", &SigncryptedMessage{
					ethSuite.NodeAddress.Hex(),
					ethSuite.NodePublicKey.X.Text(16),
					ethSuite.NodePublicKey.Y.Text(16),
					hex.EncodeToString(signcryptedOutput[i].SigncryptedShare.Ciphertext),
					signcryptedOutput[i].SigncryptedShare.R.X.Text(16),
					signcryptedOutput[i].SigncryptedShare.R.Y.Text(16),
					signcryptedOutput[i].SigncryptedShare.Signature.Text(16),
					shareIndex,
				})
				if err != nil {
					errorSlice = append(errorSlice, err)
				}
			}
		}
	}
	if errorSlice[0] == nil {
		return nil
	}
	return &errorSlice
}

func ecdsaPttoPt(ecdsaPt *ecdsa.PublicKey) *pvss.Point {
	return &pvss.Point{X: *ecdsaPt.X, Y: *ecdsaPt.Y}
}

func connectToJSONRPCNode(suite *Suite, nodeAddress common.Address) (*NodeReference, error) {
	details, err := suite.EthSuite.NodeListInstance.NodeDetails(nil, nodeAddress)
	if err != nil {
		return nil, err
	}

	// if in production use https
	var nodeIPAddress string
	if suite.Flags.Production {
		nodeIPAddress = "https://" + details.DeclaredIp + "/jrpc"
	} else {
		nodeIPAddress = "http://" + details.DeclaredIp + "/jrpc"
	}
	rpcClient := jsonrpcclient.NewClient(nodeIPAddress)

	// TODO: possible replace with signature?
	_, err = rpcClient.Call("Ping", &Message{suite.EthSuite.NodeAddress.Hex()})
	if err != nil {
		return nil, err
	}
	return &NodeReference{Address: &nodeAddress, JSONClient: rpcClient, Index: details.Position, PublicKey: &ecdsa.PublicKey{Curve: suite.EthSuite.secp, X: details.PubKx, Y: details.PubKy}}, nil
}
