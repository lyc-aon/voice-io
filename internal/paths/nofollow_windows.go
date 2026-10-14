package paths

// NoFollow is zero on Windows, which has no O_NOFOLLOW. The files it guards
// sit in the user's private %LOCALAPPDATA%, and planting a symlink there
// needs that user's rights already.
const NoFollow = 0
