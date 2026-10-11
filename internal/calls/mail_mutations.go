package calls

import (
	"errors"
	"fmt"
	"strings"

	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/lifecycle"
	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/store"
)

func (s *Service) remoteConnection(sess *session, message store.MailSummary) (*remoteimap.Client, store.Account, error) {
	if s.Store == nil || s.Secrets == nil {
		return nil, store.Account{}, fmt.Errorf("remote mail credentials are unavailable")
	}
	ctx := s.eventContext(sess)
	accounts, err := s.Store.ListAccounts(ctx, sess.UserID)
	if err != nil {
		return nil, store.Account{}, err
	}
	for _, account := range accounts {
		if account.ID != message.AccountID {
			continue
		}
		password, err := s.Secrets.Open(account.IMAPPassword)
		if err != nil {
			return nil, store.Account{}, err
		}
		port := account.IMAPPort
		if port == 0 {
			port = 993
		}
		address, err := s.resolveIMAPEndpoint(ctx, account.IMAPHost, port)
		if err != nil {
			return nil, store.Account{}, fmt.Errorf("IMAP endpoint rejected by outbound policy: %w", err)
		}
		connection, err := remoteimap.Open(ctx, remoteimap.Config{Host: account.IMAPHost, Address: address, Port: port, Security: account.IMAPSecurity, Username: account.IMAPUser, Password: password})
		if err != nil {
			return nil, store.Account{}, err
		}
		return connection, account, nil
	}
	return nil, store.Account{}, fmt.Errorf("mail account was not found")
}

func (s *Service) setRemoteSeen(sess *session, message store.MailSummary, seen bool) error {
	connection, account, err := s.remoteConnection(sess, message)
	if err != nil {
		return err
	}
	defer connection.Close()
	remoteFolderName, err := s.Store.RemoteFolderForLocal(s.eventContext(sess), sess.UserID, account.ID, message.Folder)
	if err != nil {
		return err
	}
	var mutationErr error
	if message.UID != 0 && message.UIDValidity != 0 {
		// A failed UID mutation is not safe to reinterpret as a Message-ID
		// mutation: the server may have accepted it, or UIDVALIDITY may have
		// changed. Reconciliation must resolve that state explicitly.
		mutationErr = connection.SetSeenByUID(remoteFolderName, message.UID, message.UIDValidity, seen)
	} else if strings.TrimSpace(message.MessageID) == "" {
		return fmt.Errorf("%w: remote message has no folder, UIDVALIDITY/UID pair, or Message-ID", remoteimap.ErrMissingIdentity)
	} else {
		mutationErr = connection.SetSeen(remoteFolderName, message.MessageID, seen)
	}
	if mutationErr == nil || !errors.Is(mutationErr, remoteimap.ErrMutationUncertain) {
		return mutationErr
	}
	_ = connection.Close()
	return s.reconcileRemoteSeen(sess, message, seen, mutationErr)
}

func (s *Service) moveRemote(sess *session, message store.MailSummary, destination string) error {
	connection, account, err := s.remoteConnection(sess, message)
	if err != nil {
		return err
	}
	defer connection.Close()
	source, err := s.Store.RemoteFolderForLocal(s.eventContext(sess), sess.UserID, account.ID, message.Folder)
	if err != nil {
		return err
	}
	target, err := s.Store.RemoteFolderForLocal(s.eventContext(sess), sess.UserID, account.ID, destination)
	if err != nil {
		return err
	}
	var mutationErr error
	if message.UID != 0 && message.UIDValidity != 0 {
		mutationErr = connection.MoveByUID(source, target, message.UID, message.UIDValidity)
	} else if strings.TrimSpace(message.MessageID) == "" {
		return fmt.Errorf("%w: remote message has no folder, UIDVALIDITY/UID pair, or Message-ID", remoteimap.ErrMissingIdentity)
	} else {
		mutationErr = connection.Move(source, target, message.MessageID)
	}
	if mutationErr == nil || !errors.Is(mutationErr, remoteimap.ErrMutationUncertain) {
		return mutationErr
	}
	_ = connection.Close()
	return s.reconcileRemoteMove(sess, message, destination, mutationErr)
}

func (s *Service) reconcileRemoteSeen(sess *session, message store.MailSummary, seen bool, original error) error {
	connection, account, err := s.remoteConnection(sess, message)
	if err != nil {
		return fmt.Errorf("%w: read-state confirmation connection failed after %v: %v", remoteimap.ErrMutationUncertain, original, err)
	}
	defer connection.Close()
	folder, err := s.Store.RemoteFolderForLocal(s.eventContext(sess), sess.UserID, account.ID, message.Folder)
	if err != nil {
		return fmt.Errorf("%w: read-state identity resolution failed: %v", remoteimap.ErrMutationUncertain, err)
	}
	var state remoteimap.MutationState
	if message.UID != 0 && message.UIDValidity != 0 {
		state, err = connection.ReconcileSeenByUID(folder, message.UID, message.UIDValidity, seen)
	} else if strings.TrimSpace(message.MessageID) != "" {
		state, err = connection.ReconcileSeen(folder, message.MessageID, seen)
	} else {
		return fmt.Errorf("%w: no identity available for read-state confirmation", remoteimap.ErrMutationUncertain)
	}
	if err != nil {
		return fmt.Errorf("%w: read-state confirmation failed after %v: %v", remoteimap.ErrMutationUncertain, original, err)
	}
	switch state {
	case remoteimap.MutationStateApplied:
		return nil
	case remoteimap.MutationStateNotApplied:
		return fmt.Errorf("%w: read-state change was not observed", remoteimap.ErrMutationNotApplied)
	default:
		return fmt.Errorf("%w: read-state confirmation remained inconclusive after %v", remoteimap.ErrMutationUncertain, original)
	}
}

func (s *Service) reconcileRemoteMove(sess *session, message store.MailSummary, destination string, original error) error {
	connection, account, err := s.remoteConnection(sess, message)
	if err != nil {
		return fmt.Errorf("%w: move confirmation connection failed after %v: %v", remoteimap.ErrMutationUncertain, original, err)
	}
	defer connection.Close()
	source, err := s.Store.RemoteFolderForLocal(s.eventContext(sess), sess.UserID, account.ID, message.Folder)
	if err != nil {
		return fmt.Errorf("%w: move source identity resolution failed: %v", remoteimap.ErrMutationUncertain, err)
	}
	target, err := s.Store.RemoteFolderForLocal(s.eventContext(sess), sess.UserID, account.ID, destination)
	if err != nil {
		return fmt.Errorf("%w: move destination identity resolution failed: %v", remoteimap.ErrMutationUncertain, err)
	}
	var state remoteimap.MutationState
	if message.UID != 0 && message.UIDValidity != 0 {
		// Keep reconciliation on the same stable identity used by the
		// mutation. A Message-ID may be duplicated or may belong to a
		// different message after the original UID disappeared.
		state, err = connection.ReconcileMoveByUID(source, message.UID, message.UIDValidity)
	} else if strings.TrimSpace(message.MessageID) != "" {
		state, err = connection.ReconcileMove(source, target, message.MessageID)
	} else {
		return fmt.Errorf("%w: no identity available for move confirmation", remoteimap.ErrMutationUncertain)
	}
	if err != nil {
		return fmt.Errorf("%w: move confirmation failed after %v: %v", remoteimap.ErrMutationUncertain, original, err)
	}
	switch state {
	case remoteimap.MutationStateApplied:
		return nil
	case remoteimap.MutationStateNotApplied:
		return fmt.Errorf("%w: move was not observed", remoteimap.ErrMutationNotApplied)
	default:
		return fmt.Errorf("%w: move confirmation remained inconclusive after %v", remoteimap.ErrMutationUncertain, original)
	}
}

func (s *Service) recordMutation(sess *session, message store.MailSummary, operation, fromFolder, toFolder, status string, mutationErr error) {
	if s.Store == nil || sess == nil || message.ID == 0 {
		return
	}
	cleanupCtx, cleanupCancel := lifecycle.CleanupContext(s.baseContext())
	defer cleanupCancel()
	if err := s.Store.RecordMessageMutation(cleanupCtx, message.ID, sess.UserID, operation, fromFolder, toFolder, status, mutationErr); err != nil && s.Log != nil {
		s.Log.Warn("could not record mail mutation", "message_id", message.ID, "operation", operation, "error", err)
	}
	_ = s.Store.Audit(cleanupCtx, sess.UserID, "mail_mutation", operation+":"+status)
}

func setMaildirRead(path string, read bool) (string, error) {
	return mailparse.SetMaildirRead(path, read)
}

// rollbackRemoteMove is best effort.  IMAP MOVE can change the UID, so the
// helper intentionally falls back to Message-ID when the new UID is no longer
// valid in the destination folder.
func (s *Service) rollbackRemoteMove(sess *session, message store.MailSummary, fromFolder, toFolder string) error {
	rollback := message
	rollback.Folder = fromFolder
	rollback.UID = 0
	rollback.UIDValidity = 0
	return s.moveRemote(sess, rollback, toFolder)
}
