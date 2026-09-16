package report

const reportQuery = `query Report(
  $owner: String!,
  $name: String!,
  $number: Int!,
  $states: [PullRequestReviewState!],
  $firstReviews: Int,
  $firstThreads: Int,
  $firstComments: Int,
  $reviewsAfter: String,
  $threadsAfter: String,
  $includeReviews: Boolean!,
  $includeThreads: Boolean!
) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviews(first: $firstReviews, after: $reviewsAfter, states: $states) @include(if: $includeReviews) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          state
          body
          submittedAt
          databaseId
          author { login }
        }
      }
      reviewThreads(first: $firstThreads, after: $threadsAfter) @include(if: $includeThreads) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          path
          line
          isResolved
          isOutdated
          comments(first: $firstComments) {
            pageInfo { hasNextPage endCursor }
            nodes {
              id
              databaseId
              body
              createdAt
              author { login }
              pullRequestReview {
                id
                state
                databaseId
              }
              replyTo {
                id
                databaseId
              }
            }
          }
        }
      }
    }
  }
}`

const threadCommentsQuery = `query ThreadComments(
  $threadID: ID!,
  $firstComments: Int!,
  $commentsAfter: String
) {
  node(id: $threadID) {
    ... on PullRequestReviewThread {
      comments(first: $firstComments, after: $commentsAfter) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          databaseId
          body
          createdAt
          author { login }
          pullRequestReview {
            id
            state
            databaseId
          }
          replyTo {
            id
            databaseId
          }
        }
      }
    }
  }
}`
