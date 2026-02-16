<?php

namespace App\Service;

use App\Entity\User;
use App\Entity\UserProfile;
use App\Repository\UserRepository;
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Component\PasswordHasher\Hasher\UserPasswordHasherInterface;

class UserService
{
    public function __construct(
        private EntityManagerInterface $entityManager,
        private UserRepository $userRepository,
        private UserPasswordHasherInterface $passwordHasher
    ) {}

    /**
     * @return array{users: array<array<string, mixed>>, total: int}
     */
    public function listUsers(int $page = 1, int $limit = 25, ?string $search = null): array
    {
        $queryBuilder = $this->userRepository->createQueryBuilder('u')
            ->leftJoin('u.profile', 'p')
            ->addSelect('p')
            ->orderBy('u.createdAt', 'DESC');

        if ($search !== null && $search !== '') {
            $queryBuilder
                ->andWhere('u.username LIKE :search OR u.email LIKE :search OR p.alias LIKE :search')
                ->setParameter('search', '%' . $search . '%');
        }

        $countQuery = (clone $queryBuilder)
            ->select('COUNT(u.id)')
            ->resetDQLPart('orderBy');
        $total = $countQuery->getQuery()->getSingleScalarResult();

        $users = $queryBuilder
            ->setFirstResult(($page - 1) * $limit)
            ->setMaxResults($limit)
            ->getQuery()
            ->getResult();

        return [
            'users' => array_map([$this, 'serializeUser'], $users),
            'total' => (int) $total,
        ];
    }

    /**
     * @return array<string, mixed>
     */
    public function getUser(string $id): ?array
    {
        $user = $this->userRepository->find($id);
        if ($user === null) {
            return null;
        }

        return $this->serializeUserDetail($user);
    }

    /**
     * @param array<string, mixed> $data
     * @return array<string, mixed>
     */
    public function createUser(array $data): array
    {
        $user = new User();
        $user->setUsername($data['username']);
        $user->setPassword($this->passwordHasher->hashPassword($user, $data['password']));

        if (isset($data['email']) && $data['email'] !== '') {
            $user->setEmail($data['email']);
        }

        if (isset($data['roles']) && is_array($data['roles'])) {
            $user->setRoles($data['roles']);
        }

        if (isset($data['isActive'])) {
            $user->setIsActive((bool) $data['isActive']);
        }

        $profile = new UserProfile();
        $profile->setUser($user);
        $user->setProfile($profile);

        $this->entityManager->persist($user);
        $this->entityManager->persist($profile);
        $this->entityManager->flush();

        return $this->serializeUserDetail($user);
    }

    /**
     * @param array<string, mixed> $data
     * @return array<string, mixed>|null
     */
    public function updateUser(string $id, array $data): ?array
    {
        $user = $this->userRepository->find($id);
        if ($user === null) {
            return null;
        }

        if (isset($data['email'])) {
            $user->setEmail($data['email'] !== '' ? $data['email'] : null);
        }

        if (isset($data['roles']) && is_array($data['roles'])) {
            $user->setRoles($data['roles']);
        }

        if (isset($data['isActive'])) {
            $user->setIsActive((bool) $data['isActive']);
        }

        if (isset($data['password']) && $data['password'] !== '') {
            $user->setPassword($this->passwordHasher->hashPassword($user, $data['password']));
        }

        $profile = $user->getProfile();
        if ($profile !== null) {
            if (array_key_exists('alias', $data)) {
                $profile->setAlias($data['alias'] !== '' ? $data['alias'] : null);
            }

            if (array_key_exists('bio', $data)) {
                $profile->setBio($data['bio'] !== '' ? $data['bio'] : null);
            }

            if (array_key_exists('entryMessage', $data)) {
                $profile->setEntryMessage($data['entryMessage'] !== '' ? $data['entryMessage'] : null);
            }

            if (array_key_exists('exitMessage', $data)) {
                $profile->setExitMessage($data['exitMessage'] !== '' ? $data['exitMessage'] : null);
            }

            if (array_key_exists('preferredColor', $data)) {
                $profile->setPreferredColor($data['preferredColor'] !== '' ? $data['preferredColor'] : null);
            }

            if (array_key_exists('timezone', $data)) {
                $profile->setTimezone($data['timezone'] !== '' ? $data['timezone'] : null);
            }
        }

        $this->entityManager->flush();

        return $this->serializeUserDetail($user);
    }

    public function deleteUser(string $id): bool
    {
        $user = $this->userRepository->find($id);
        if ($user === null) {
            return false;
        }

        $this->entityManager->remove($user);
        $this->entityManager->flush();

        return true;
    }

    /**
     * @return array<string, mixed>
     */
    private function serializeUser(User $user): array
    {
        return [
            'id' => $user->getId()->toRfc4122(),
            'username' => $user->getUsername(),
            'email' => $user->getEmail(),
            'roles' => $user->getRoles(),
            'isActive' => $user->isActive(),
            'createdAt' => $user->getCreatedAt()->format('c'),
            'lastLoginAt' => $user->getLastLoginAt()?->format('c'),
            'alias' => $user->getProfile()?->getDisplayName(),
        ];
    }

    /**
     * @return array<string, mixed>
     */
    private function serializeUserDetail(User $user): array
    {
        $data = $this->serializeUser($user);
        $profile = $user->getProfile();

        if ($profile !== null) {
            $data['profile'] = [
                'alias' => $profile->getAlias(),
                'gender' => $profile->getGender()->value,
                'entryMessage' => $profile->getEntryMessage(),
                'exitMessage' => $profile->getExitMessage(),
                'bio' => $profile->getBio(),
                'avatarUrl' => $profile->getAvatarUrl(),
                'preferredColor' => $profile->getPreferredColor(),
                'timezone' => $profile->getTimezone(),
            ];
        }

        return $data;
    }
}
