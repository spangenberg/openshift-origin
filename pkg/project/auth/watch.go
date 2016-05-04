package auth

import (
	"errors"
	"sync"

	"github.com/golang/glog"

	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/auth/user"
	"k8s.io/kubernetes/pkg/storage/etcd"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/kubernetes/pkg/watch"

	projectcache "github.com/openshift/origin/pkg/project/cache"
	projectutil "github.com/openshift/origin/pkg/project/util"
)

type CacheWatcher interface {
	// GroupMembershipChanged is called serially for all changes for all watchers.  This method MUST NOT BLOCK.
	// The serial nature makes reasoning about the code easy, but if you block in this method you will doom all watchers.
	GroupMembershipChanged(namespaceName string, latestUsers, latestGroups, removedUsers, removedGroups, addedUsers, addedGroups sets.String)
}

type WatchableCache interface {
	// RemoveWatcher removes a watcher
	RemoveWatcher(CacheWatcher)
	// List returns the set of namespace names the user has access to view
	List(userInfo user.Info) (*kapi.NamespaceList, error)
}

// userProjectWatcher converts a native etcd watch to a watch.Interface.
type userProjectWatcher struct {
	username string
	groups   []string

	// cacheIncoming is a buffered channel used for notification to watcher.  If the buffer fills up,
	// then the watcher will be removed and the connection will be broken.
	cacheIncoming chan watch.Event
	// cacheError is a cached channel that is put to serially.  In theory, only one item will
	// ever be placed on it.
	cacheError chan error

	// outgoing is the unbuffered `ResultChan` use for the watch.  Backups of this channel will block
	// the default `emit` call.  That's why cacheError is a buffered channel.
	outgoing chan watch.Event
	// userStop lets a user stop his watch.
	userStop chan struct{}

	// stopLock keeps parallel stops from doing crazy things
	stopLock sync.Mutex

	// Injectable for testing. Send the event down the outgoing channel.
	emit func(watch.Event)

	projectCache *projectcache.ProjectCache
	authCache    WatchableCache

	initialProjects []kapi.Namespace
	knownProjects   sets.String
}

var (
	// watchChannelHWM tracks how backed up the most backed up channel got.  This mirrors etcd watch behavior and allows tuning
	// of channel depth.
	watchChannelHWM etcd.HighWaterMark
)

func NewUserProjectWatcher(username string, groups []string, projectCache *projectcache.ProjectCache, authCache WatchableCache, includeAllExistingProjects bool) *userProjectWatcher {
	userInfo := &user.DefaultInfo{Name: username, Groups: groups}
	namespaces, _ := authCache.List(userInfo)
	knownProjects := sets.String{}
	for _, namespace := range namespaces.Items {
		knownProjects.Insert(namespace.Name)
	}

	// this is optional.  If they don't request it, don't include it.
	initialProjects := []kapi.Namespace{}
	if includeAllExistingProjects {
		initialProjects = append(initialProjects, namespaces.Items...)
	}

	w := &userProjectWatcher{
		username: username,
		groups:   groups,

		cacheIncoming: make(chan watch.Event, 1000),
		cacheError:    make(chan error, 1),
		outgoing:      make(chan watch.Event),
		userStop:      make(chan struct{}),

		projectCache:    projectCache,
		authCache:       authCache,
		initialProjects: initialProjects,
		knownProjects:   knownProjects,
	}
	w.emit = func(e watch.Event) {
		select {
		case w.outgoing <- e:
		case <-w.userStop:
		}
	}
	return w
}

func (w *userProjectWatcher) GroupMembershipChanged(namespaceName string, latestUsers, lastestGroups, removedUsers, removedGroups, addedUsers, addedGroups sets.String) {
	hasAccess := latestUsers.Has(w.username) || lastestGroups.HasAny(w.groups...)
	removed := !hasAccess && (removedUsers.Has(w.username) || removedGroups.HasAny(w.groups...))

	switch {
	case removed:
		if !w.knownProjects.Has(namespaceName) {
			return
		}
		w.knownProjects.Delete(namespaceName)

		select {
		case w.cacheIncoming <- watch.Event{
			Type:   watch.Deleted,
			Object: projectutil.ConvertNamespace(&kapi.Namespace{ObjectMeta: kapi.ObjectMeta{Name: namespaceName}}),
		}:
		default:
			// remove the watcher so that we wont' be notified again and block
			w.authCache.RemoveWatcher(w)
			w.cacheError <- errors.New("delete notification timeout")
		}

	case hasAccess:
		namespace, err := w.projectCache.GetNamespace(namespaceName)
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		event := watch.Event{
			Type:   watch.Added,
			Object: projectutil.ConvertNamespace(namespace),
		}

		// if we already have this in our list, then we're getting notified because the object changed
		if w.knownProjects.Has(namespaceName) {
			event.Type = watch.Modified
		}
		w.knownProjects.Insert(namespace.Name)

		select {
		case w.cacheIncoming <- event:
		default:
			// remove the watcher so that we won't be notified again and block
			w.authCache.RemoveWatcher(w)
			w.cacheError <- errors.New("add notification timeout")
		}

	}

}

// Watch pulls stuff from etcd, converts, and pushes out the outgoing channel. Meant to be
// called as a goroutine.
func (w *userProjectWatcher) Watch() {
	defer close(w.outgoing)
	defer func() {
		// when the watch ends, always remove the watcher from the cache to avoid leaking.
		w.authCache.RemoveWatcher(w)
	}()
	defer utilruntime.HandleCrash()

	// start by emitting all the `initialProjects`
	for i := range w.initialProjects {
		// keep this check here to sure we don't keep this open in the case of failures
		select {
		case err := <-w.cacheError:
			w.emit(makeErrorEvent(err))
			return
		default:
		}

		w.emit(watch.Event{
			Type:   watch.Added,
			Object: projectutil.ConvertNamespace(&w.initialProjects[i]),
		})
	}

	for {
		select {
		case err := <-w.cacheError:
			w.emit(makeErrorEvent(err))
			return

		case <-w.userStop:
			return

		case event := <-w.cacheIncoming:
			if curLen := int64(len(w.cacheIncoming)); watchChannelHWM.Update(curLen) {
				// Monitor if this gets backed up, and how much.
				glog.V(2).Infof("watch: %v objects queued in project cache watching channel.", curLen)
			}

			w.emit(event)
		}
	}
}

func makeErrorEvent(err error) watch.Event {
	return watch.Event{
		Type: watch.Error,
		Object: &unversioned.Status{
			Status:  unversioned.StatusFailure,
			Message: err.Error(),
		},
	}
}

// ResultChan implements watch.Interface.
func (w *userProjectWatcher) ResultChan() <-chan watch.Event {
	return w.outgoing
}

// Stop implements watch.Interface.
func (w *userProjectWatcher) Stop() {
	// lock access so we don't race past the channel select
	w.stopLock.Lock()
	defer w.stopLock.Unlock()

	// Prevent double channel closes.
	select {
	case <-w.userStop:
		return
	default:
	}
	close(w.userStop)
}
